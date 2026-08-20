// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/routing"
)

// fakeSender is a minimal llm.Sender stub: it returns queued canned
// responses in order (the last repeats) and records every request it saw.
type fakeSender struct {
	calls   int
	scripts []func(anthropic.MessageNewParams) (*anthropic.Message, error)
	lastReq []anthropic.MessageNewParams
}

func (s *fakeSender) New(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.lastReq = append(s.lastReq, params)
	i := s.calls
	if i >= len(s.scripts) {
		i = len(s.scripts) - 1
	}
	s.calls++
	return s.scripts[i](params)
}

func cannedMessage(t *testing.T, raw string) *anthropic.Message {
	t.Helper()
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("cannedMessage: %v", err)
	}
	return &m
}

// respondToolUse returns a script step that replies with a single
// report_findings tool_use block carrying inputJSON.
func respondToolUse(t *testing.T, inputJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[{"type":"tool_use","id":"tu_1","name":"report_findings","input":` + inputJSON + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`
	msg := cannedMessage(t, raw)
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return msg, nil }
}

// respondTextOnly returns a script step that replies with plain text (no
// tool_use block) — the "detector didn't call the tool" malformed case.
func respondTextOnly(text string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
			`"content":[{"type":"text","text":"` + text + `"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50}}`
		var m anthropic.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			panic(err)
		}
		return &m, nil
	}
}

func testDetectClient(t *testing.T, sender llm.Sender) *llm.Client {
	t.Helper()
	c, err := llm.NewClient(llm.WithProviderSender("anthropic", sender))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func fixtureTranscript() *insights.Transcript {
	textContent := func(s string) json.RawMessage {
		b, _ := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		return b
	}
	return &insights.Transcript{
		ConversationID:  "conv-1",
		Owner:           "brent",
		Persona:         "diagnose",
		Source:          "claude",
		AvailableSkills: []string{"sc-diagnose-replication-lag"},
		Turns: []insights.TranscriptTurn{
			{Ordinal: 0, Role: "user", Content: textContent("why is replica X lagging?")},
			{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: 10, OutputTokens: 5},
		},
	}
}

func fakePricer(prompt, completion float64) insights.Pricer {
	return func(model string) (routing.ModelPricing, bool) {
		return routing.ModelPricing{PromptUSD: prompt, CompletionUSD: completion}, true
	}
}

const wellFormedInput = `{
	"outcome": "agent diagnosed replication lag from a stuck WAL sender",
	"verdicts": {"skill-gap": "ok", "knowledge-to-persist": "finding", "grind": "ok"},
	"findings": [{
		"axis": "knowledge-to-persist",
		"title": "WAL sender stuck behind a long-running query on the replica",
		"topic_key": "wal-sender-stuck-long-query",
		"evidence": [{"ordinal": 1, "quote": "investigating..."}],
		"recommendation": {"kind": "memory", "summary": "record the diagnosis pattern"},
		"confidence": 0.8
	}]
}`

func TestDetectWellFormedToolUse(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", fakePricer(0.001, 0.002))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if analysis.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want conv-1", analysis.ConversationID)
	}
	if analysis.DetectorVersion != DetectorVersion {
		t.Errorf("DetectorVersion = %d, want %d", analysis.DetectorVersion, DetectorVersion)
	}
	if analysis.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", analysis.Model)
	}
	if analysis.InputTokens != 100 || analysis.OutputTokens != 50 {
		t.Errorf("tokens = in=%d out=%d, want in=100 out=50", analysis.InputTokens, analysis.OutputTokens)
	}
	wantCost := 100*0.001 + 50*0.002
	if analysis.CostUSD != wantCost {
		t.Errorf("CostUSD = %v, want %v", analysis.CostUSD, wantCost)
	}
	if analysis.Outcome == "" {
		t.Error("Outcome empty")
	}
	if len(analysis.Findings) != 1 || analysis.Findings[0].Axis != "knowledge-to-persist" {
		t.Errorf("Findings = %+v, want one knowledge-to-persist finding", analysis.Findings)
	}
	if analysis.Verdicts["skill-gap"] != "ok" {
		t.Errorf("Verdicts[skill-gap] = %q, want ok", analysis.Verdicts["skill-gap"])
	}

	if len(sender.lastReq) != 1 {
		t.Fatalf("requests sent = %d, want 1 (no retry needed)", len(sender.lastReq))
	}
	if sender.lastReq[0].ToolChoice.OfTool == nil || sender.lastReq[0].ToolChoice.OfTool.Name != "report_findings" {
		t.Errorf("request did not force report_findings tool choice: %+v", sender.lastReq[0].ToolChoice)
	}

	if analysis.PromptHash != "" {
		t.Errorf("PromptHash = %q, want \"\" for a default profile (builtin prompts)", analysis.PromptHash)
	}
}

func TestDetectSendsProfileMaxOutputTokens(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5", MaxOutputTokens: 8871}

	if _, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if len(sender.lastReq) != 1 {
		t.Fatalf("requests sent = %d, want 1", len(sender.lastReq))
	}
	if got := sender.lastReq[0].MaxTokens; got != 8871 {
		t.Errorf("MaxTokens = %d, want 8871", got)
	}
}

func TestDetectRecordsPromptHash(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5", DetectorPromptExtra: "also flag missing runbooks"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if analysis.PromptHash == "" {
		t.Fatal("PromptHash empty, want non-empty for a profile with DetectorPromptExtra set")
	}
	if analysis.PromptHash != p.PromptHash() {
		t.Errorf("PromptHash = %q, want %q (p.PromptHash())", analysis.PromptHash, p.PromptHash())
	}
}

func TestDetectRetriesOnceOnMalformedResponse(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("I looked at the conversation but forgot to call the tool"),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if analysis.Outcome == "" {
		t.Error("Outcome empty after retry")
	}
	if analysis.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 with nil pricer", analysis.CostUSD)
	}

	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry)", len(sender.lastReq))
	}

	// The retry's new user turn must carry the parse error text.
	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if lastMsg.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message in retry request is role %q, want user", lastMsg.Role)
	}
	var found bool
	for _, block := range lastMsg.Content {
		if block.OfText != nil && strings.Contains(block.OfText.Text, "no report_findings tool_use block") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request's last user turn did not contain the parse error; content=%+v", lastMsg.Content)
	}
}

const invalidAxisInput = `{
	"outcome": "did something",
	"verdicts": {"skill-gap": "ok", "knowledge-to-persist": "finding", "grind": "ok"},
	"findings": [{
		"axis": "not-a-real-axis",
		"title": "bogus finding",
		"topic_key": "bogus",
		"evidence": [{"ordinal": 1, "quote": "x"}],
		"recommendation": {"kind": "memory", "summary": "y"},
		"confidence": 0.5
	}]
}`

// TestDetectRetriesWithToolResultOnInvalidEnum covers the case the real
// Anthropic API rejects: the first response DOES contain a report_findings
// tool_use (with a schema-valid-but-semantically-invalid axis enum), so the
// retry must answer that dangling tool_use with a tool_result block
// referencing its ID — not a plain user-text turn.
func TestDetectRetriesWithToolResultOnInvalidEnum(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, invalidAxisInput),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if analysis.Outcome == "" {
		t.Error("Outcome empty after retry")
	}

	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry)", len(sender.lastReq))
	}

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if lastMsg.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message in retry request is role %q, want user", lastMsg.Role)
	}
	if len(lastMsg.Content) != 1 || lastMsg.Content[0].OfToolResult == nil {
		t.Fatalf("retry request's last user turn = %+v, want a single tool_result block "+
			"(a dangling tool_use must be answered, not followed by plain text)", lastMsg.Content)
	}
	tr := lastMsg.Content[0].OfToolResult
	if tr.ToolUseID != "tu_1" {
		t.Errorf("tool_result.tool_use_id = %q, want tu_1 (the first response's tool_use id)", tr.ToolUseID)
	}
	if !tr.IsError.Value {
		t.Error("tool_result.is_error = false, want true")
	}
}

// respondTwoToolUse returns a script step that replies with TWO
// report_findings tool_use blocks (parallel tool use) — both with
// inputJSON, both needing a tool_result on retry.
func respondTwoToolUse(t *testing.T, inputJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[` +
		`{"type":"tool_use","id":"tu_1","name":"report_findings","input":` + inputJSON + `},` +
		`{"type":"tool_use","id":"tu_2","name":"report_findings","input":` + inputJSON + `}` +
		`],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`
	msg := cannedMessage(t, raw)
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return msg, nil }
}

// TestDetectRetriesAnswersAllToolUseBlocks covers parallel tool use: the
// first response contains TWO report_findings tool_use blocks (both
// invalid), so the retry must answer BOTH dangling tool_use ids with
// tool_result blocks — leaving either unanswered still 400s against the
// real Anthropic API.
func TestDetectRetriesAnswersAllToolUseBlocks(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTwoToolUse(t, invalidAxisInput),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if analysis.Outcome == "" {
		t.Error("Outcome empty after retry")
	}

	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry)", len(sender.lastReq))
	}

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if lastMsg.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message in retry request is role %q, want user", lastMsg.Role)
	}
	if len(lastMsg.Content) != 2 {
		t.Fatalf("retry request's last user turn has %d blocks, want 2 (a tool_result for EACH dangling tool_use)",
			len(lastMsg.Content))
	}
	gotIDs := map[string]bool{}
	for _, block := range lastMsg.Content {
		if block.OfToolResult == nil {
			t.Fatalf("retry request block = %+v, want tool_result", block)
		}
		if !block.OfToolResult.IsError.Value {
			t.Errorf("tool_result(%s).is_error = false, want true", block.OfToolResult.ToolUseID)
		}
		gotIDs[block.OfToolResult.ToolUseID] = true
	}
	if !gotIDs["tu_1"] || !gotIDs["tu_2"] {
		t.Errorf("retry request tool_result ids = %v, want both tu_1 and tu_2", gotIDs)
	}
}

func TestDetectFailsAfterTwoMalformedResponses(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("first malformed reply"),
		respondTextOnly("second malformed reply"),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	_, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	if err == nil {
		t.Fatal("Detect: want error after two malformed responses, got nil")
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (initial + one retry, no third attempt)", len(sender.lastReq))
	}
}
