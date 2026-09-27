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

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(json.Unmarshal([]byte(raw), &m), "cannedMessage")
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
	assert.NewAborting(t).NoError(err, "NewClient")
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
			{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: i64(10), OutputTokens: i64(5)},
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
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", fakePricer(0.001, 0.002))
	ck.Require().NoError(err, "Detect")

	ck.Eq("conv-1", analysis.ConversationID, "ConversationID")
	ck.Eq(DetectorVersion, analysis.DetectorVersion, "DetectorVersion")
	ck.Eq("claude-haiku-4-5", analysis.Model, "Model")
	if analysis.InputTokens != 100 || analysis.OutputTokens != 50 {
		t.Errorf("tokens = in=%d out=%d, want in=100 out=50", analysis.InputTokens, analysis.OutputTokens)
	}
	wantCost := 100*0.001 + 50*0.002
	ck.Eq(wantCost, analysis.CostUSD, "CostUSD")
	ck.NotEq("", analysis.Outcome, "Outcome empty")
	if len(analysis.Findings) != 1 || analysis.Findings[0].Axis != "knowledge-to-persist" {
		t.Errorf("Findings = %+v, want one knowledge-to-persist finding", analysis.Findings)
	}
	ck.Eq("ok", analysis.Verdicts["skill-gap"], "Verdicts[skill-gap]")

	ck.Require().Len(sender.lastReq, 1, "requests sent = %d, want 1 (no retry needed)", len(sender.lastReq))
	if sender.lastReq[0].ToolChoice.OfTool == nil || sender.lastReq[0].ToolChoice.OfTool.Name != "report_findings" {
		t.Errorf("request did not force report_findings tool choice: %+v", sender.lastReq[0].ToolChoice)
	}

	ck.Eq("", analysis.PromptHash, "PromptHash = %q, want \"\" for a default profile (builtin prompts)", analysis.PromptHash)
}

func TestDetectSendsProfileMaxOutputTokens(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5", MaxOutputTokens: 8871}

	_, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Require().NoError(err, "Detect")

	ck.Require().Len(sender.lastReq, 1, "requests sent = %d, want 1", len(sender.lastReq))
	ck.Eq(8871, sender.lastReq[0].MaxTokens, "MaxTokens")
}

func TestDetectRecordsPromptHash(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5", DetectorPromptExtra: "also flag missing runbooks"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Require().NoError(err, "Detect")
	ck.Require().NotEq("", analysis.PromptHash, "PromptHash empty, want non-empty for a profile with DetectorPromptExtra set")
	ck.Eq(p.PromptHash(), analysis.PromptHash, "PromptHash")
}

func TestDetectRetriesOnceOnMalformedResponse(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("I looked at the conversation but forgot to call the tool"),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Require().NoError(err, "Detect")
	ck.NotEq("", analysis.Outcome, "Outcome empty after retry")
	ck.Eq(0, analysis.CostUSD, "CostUSD")

	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry)", len(sender.lastReq))

	// The retry's new user turn must carry the parse error text.
	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Eq(anthropic.MessageParamRoleUser, lastMsg.Role, "last message in retry request is role")
	var found bool
	for _, block := range lastMsg.Content {
		if block.OfText != nil && strings.Contains(block.OfText.Text, "no report_findings tool_use block") {
			found = true
		}
	}
	ck.True(found, "retry request's last user turn did not contain the parse error; content=%+v", lastMsg.Content)
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
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondToolUse(t, invalidAxisInput),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Require().NoError(err, "Detect")
	ck.NotEq("", analysis.Outcome, "Outcome empty after retry")

	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry)", len(sender.lastReq))

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Eq(anthropic.MessageParamRoleUser, lastMsg.Role, "last message in retry request is role")
	if len(lastMsg.Content) != 1 || lastMsg.Content[0].OfToolResult == nil {
		t.Fatalf("retry request's last user turn = %+v, want a single tool_result block "+
			"(a dangling tool_use must be answered, not followed by plain text)", lastMsg.Content)
	}
	tr := lastMsg.Content[0].OfToolResult
	ck.Eq("tu_1", tr.ToolUseID, "tool_result.tool_use_id")
	ck.True(tr.IsError.Value, "tool_result.is_error = false, want true")
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
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTwoToolUse(t, invalidAxisInput),
		respondToolUse(t, wellFormedInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	analysis, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Require().NoError(err, "Detect")
	ck.NotEq("", analysis.Outcome, "Outcome empty after retry")

	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry)", len(sender.lastReq))

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Eq(anthropic.MessageParamRoleUser, lastMsg.Role, "last message in retry request is role")
	ck.Require().Len(lastMsg.Content, 2, "retry request's last user turn has %d blocks, want 2 (a tool_result for EACH dangling tool_use)", len(lastMsg.Content))
	gotIDs := map[string]bool{}
	for _, block := range lastMsg.Content {
		ck.Require().NotNil(block.OfToolResult, "retry request block = %+v, want tool_result", block)
		if !block.OfToolResult.IsError.Value {
			t.Errorf("tool_result(%s).is_error = false, want true", block.OfToolResult.ToolUseID)
		}
		gotIDs[block.OfToolResult.ToolUseID] = true
	}
	ck.False(!gotIDs["tu_1"] || !gotIDs["tu_2"], "retry request tool_result ids = %v, want both tu_1 and tu_2", gotIDs)
}

func TestDetectFailsAfterTwoMalformedResponses(t *testing.T) {
	ck := assert.NewAborting(t)
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("first malformed reply"),
		respondTextOnly("second malformed reply"),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DetectorModel: "claude-haiku-4-5"}

	_, err := Detect(context.Background(), c, fixtureTranscript(), p, "brent", nil)
	ck.Error(err, "Detect: want error after two malformed responses, got nil")
	ck.Len(sender.lastReq, 2, "requests sent = %d, want 2 (initial + one retry, no third attempt)", len(sender.lastReq))
}

// TestRenderTranscriptMarkdownMetricsOnlyWhenReported pins the renderer's nil
// guard: a turn with unreported metrics (a user or pre-fill row) prints no
// token line, while a measured zero still prints as zero.
func TestRenderTranscriptMarkdownMetricsOnlyWhenReported(t *testing.T) {
	c := assert.NewCollecting(t)
	zero, forty := int64(0), int64(40)
	md := renderTranscriptMarkdown(&insights.Transcript{Turns: []insights.TranscriptTurn{
		{Ordinal: 0, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		{Ordinal: 1, Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"ok"}]`),
			InputTokens: &forty, OutputTokens: &zero},
	}})
	c.Eq(1, strings.Count(md, "in="), "want exactly one token line (the reported turn), got:\n%s", md)
	c.StrContains(md, "in=40 out=0", "a measured zero must still render, got:\n")
}

func i64(v int64) *int64 { return &v }
