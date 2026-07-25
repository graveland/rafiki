package analyze

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// respondDraftToolUse returns a script step that replies with a single
// propose_skill_edit tool_use block carrying inputJSON.
func respondDraftToolUse(t *testing.T, inputJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[{"type":"tool_use","id":"tu_1","name":"propose_skill_edit","input":` + inputJSON + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`
	msg := cannedMessage(t, raw)
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return msg, nil }
}

func fixtureRankedFinding() RankedFinding {
	return RankedFinding{
		Finding: Finding{
			Axis:     "skill-gap",
			Title:    "no skill for diagnosing replication lag",
			TopicKey: "missing-replication-lag-skill",
			Evidence: []TurnCite{{Ordinal: 1, Quote: "investigating replication lag manually..."}},
			Recommendation: Recommendation{
				Kind:      "skill-edit",
				SkillName: "sc-diagnose-replication-lag",
				Summary:   "extend the skill's trigger description",
			},
			Confidence: 0.8,
		},
		Conversations: []string{"conv-1"},
		Occurrences:   1,
		Score:         500,
	}
}

const editDraftInput = `{
	"files": [{"path": "skills/sc-diagnose-replication-lag/SKILL.md", "content": "---\nname: sc-diagnose-replication-lag\ndescription: updated trigger\n---\n\nbody"}],
	"rationale": "broadened the trigger description"
}`

func TestDraftEditPathCarriesSamePathAndFindingTitle(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	current := []SkillFile{{
		Path:    "skills/sc-diagnose-replication-lag/SKILL.md",
		Content: "---\nname: sc-diagnose-replication-lag\ndescription: old trigger\n---\n\nbody",
	}}
	f := fixtureRankedFinding()

	edit, err := Draft(context.Background(), c, f, current, p, "brent", nil)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if edit.FindingTitle != f.Title {
		t.Errorf("FindingTitle = %q, want %q", edit.FindingTitle, f.Title)
	}
	if len(edit.Files) != 1 || edit.Files[0].Path != current[0].Path {
		t.Fatalf("Files = %+v, want one file at %q", edit.Files, current[0].Path)
	}
	if edit.Files[0].Content == current[0].Content {
		t.Error("Files[0].Content unchanged, want modified content from the response")
	}
	if edit.Rationale == "" {
		t.Error("Rationale empty")
	}

	if len(sender.lastReq) != 1 {
		t.Fatalf("requests sent = %d, want 1", len(sender.lastReq))
	}
	if sender.lastReq[0].ToolChoice.OfTool == nil || sender.lastReq[0].ToolChoice.OfTool.Name != "propose_skill_edit" {
		t.Errorf("request did not force propose_skill_edit tool choice: %+v", sender.lastReq[0].ToolChoice)
	}

	// The user turn must have surfaced the existing file content so the
	// model could edit it minimally.
	userMsg := sender.lastReq[0].Messages[len(sender.lastReq[0].Messages)-1]
	var sawCurrentContent bool
	for _, block := range userMsg.Content {
		if block.OfText != nil && strings.Contains(block.OfText.Text, "old trigger") {
			sawCurrentContent = true
		}
	}
	if !sawCurrentContent {
		t.Error("request did not include the current file's content")
	}
}

const newSkillDraftInput = `{
	"files": [{"path": "skills/sc-diagnose-replication-lag/SKILL.md", "content": "---\nname: sc-diagnose-replication-lag\ndescription: new skill\n---\n\nbody"}],
	"rationale": "no existing skill covered this, so created one"
}`

func TestDraftNewSkillPathInstructsSkillsDirLayout(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, newSkillDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	f := fixtureRankedFinding()
	edit, err := Draft(context.Background(), c, f, nil, p, "brent", nil)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if edit.FindingTitle != f.Title {
		t.Errorf("FindingTitle = %q, want %q", edit.FindingTitle, f.Title)
	}
	if len(edit.Files) != 1 {
		t.Fatalf("Files = %+v, want one file", edit.Files)
	}
	if !strings.HasPrefix(edit.Files[0].Path, "skills/") || !strings.HasSuffix(edit.Files[0].Path, "SKILL.md") {
		t.Errorf("Files[0].Path = %q, want under skills/<name>/SKILL.md", edit.Files[0].Path)
	}

	userMsg := sender.lastReq[0].Messages[len(sender.lastReq[0].Messages)-1]
	var sawNewSkillInstruction bool
	for _, block := range userMsg.Content {
		if block.OfText != nil && strings.Contains(block.OfText.Text, "new skill") &&
			strings.Contains(block.OfText.Text, "skills/<skill-name>/SKILL.md") {
			sawNewSkillInstruction = true
		}
	}
	if !sawNewSkillInstruction {
		t.Error("request did not instruct the new-skill skills/<name>/SKILL.md layout")
	}
}

func TestDraftRetriesOnceOnMalformedResponse(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("forgot to call the tool"),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if len(edit.Files) == 0 {
		t.Error("Files empty after retry")
	}

	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry)", len(sender.lastReq))
	}

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if lastMsg.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message in retry request is role %q, want user", lastMsg.Role)
	}
	var found bool
	for _, block := range lastMsg.Content {
		if block.OfText != nil && strings.Contains(block.OfText.Text, "no propose_skill_edit tool_use block") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request's last user turn did not contain the parse error; content=%+v", lastMsg.Content)
	}
}

func TestDraftFailsAfterTwoMalformedResponses(t *testing.T) {
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondTextOnly("first malformed reply"),
		respondTextOnly("second malformed reply"),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err == nil {
		t.Fatal("Draft: want error after two malformed responses, got nil")
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (initial + one retry, no third attempt)", len(sender.lastReq))
	}
}

func TestDraftRejectsAbsolutePathAndRetries(t *testing.T) {
	invalidInput := `{
		"files": [{"path": "/etc/skills/SKILL.md", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if len(edit.Files) == 0 {
		t.Error("Files empty after retry")
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry after invalid path)", len(sender.lastReq))
	}

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if lastMsg.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message in retry request is role %q, want user", lastMsg.Role)
	}
	// The first response DID call propose_skill_edit (with invalid input),
	// so the retry must answer that dangling tool_use with a tool_result —
	// a plain user-text turn here is what the real Anthropic API rejects.
	if len(lastMsg.Content) != 1 || lastMsg.Content[0].OfToolResult == nil {
		t.Fatalf("retry request's last user turn = %+v, want a single tool_result block", lastMsg.Content)
	}
	tr := lastMsg.Content[0].OfToolResult
	if tr.ToolUseID != "tu_1" {
		t.Errorf("tool_result.tool_use_id = %q, want tu_1 (the first response's tool_use id)", tr.ToolUseID)
	}
	if !tr.IsError.Value {
		t.Error("tool_result.is_error = false, want true")
	}
	var found bool
	for _, sub := range tr.Content {
		if sub.OfText != nil && strings.Contains(sub.OfText.Text, "must be relative") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request did not surface the absolute-path validation error; tool_result content=%+v", tr.Content)
	}
}

func TestDraftRejectsBackslashPathAndRetries(t *testing.T) {
	invalidInput := `{
		"files": [{"path": "..\\\\..\\\\etc\\\\passwd", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if len(edit.Files) == 0 {
		t.Error("Files empty after retry")
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (one retry after backslash path)", len(sender.lastReq))
	}

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	if len(lastMsg.Content) != 1 || lastMsg.Content[0].OfToolResult == nil {
		t.Fatalf("retry request's last user turn = %+v, want a single tool_result block", lastMsg.Content)
	}
	tr := lastMsg.Content[0].OfToolResult
	var found bool
	for _, sub := range tr.Content {
		if sub.OfText != nil && strings.Contains(sub.OfText.Text, "\\\\") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request did not surface the backslash-path validation error; tool_result content=%+v", tr.Content)
	}
}

func TestDraftRejectsNormalizedTraversalPathAndFailsAfterRetry(t *testing.T) {
	// "a/../../b" contains two literal ".." segments; the segment scan
	// rejects it outright regardless of what it would normalize to.
	invalidInput := `{
		"files": [{"path": "a/../../b", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, invalidInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err == nil {
		t.Fatal("Draft: want error, got nil")
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (initial + one retry)", len(sender.lastReq))
	}
}

func TestDraftRejectsDotDotPathAndFailsAfterRetry(t *testing.T) {
	invalidInput := `{
		"files": [{"path": "skills/../../etc/passwd", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, invalidInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	if err == nil {
		t.Fatal("Draft: want error, got nil")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("error = %v, want mention of \"..\" segment rejection", err)
	}
	if len(sender.lastReq) != 2 {
		t.Fatalf("requests sent = %d, want 2 (initial + one retry)", len(sender.lastReq))
	}
}
