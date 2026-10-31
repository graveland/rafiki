// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// respondDraftToolUse returns a script step that replies with a single
// propose_skill_edit tool_use block carrying inputJSON.
func respondDraftToolUse(t *testing.T, inputJSON string) func(*msg.Request) (*msg.Response, error) {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[{"type":"tool_use","id":"tu_1","name":"propose_skill_edit","input":` + inputJSON + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`
	resp := cannedMessage(t, raw)
	return func(*msg.Request) (*msg.Response, error) { return resp, nil }
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
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
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
	ck.Require().NoError(err, "Draft")
	ck.Eq(f.Title, edit.FindingTitle, "FindingTitle")
	if len(edit.Files) != 1 || edit.Files[0].Path != current[0].Path {
		t.Fatalf("Files = %+v, want one file at %q", edit.Files, current[0].Path)
	}
	ck.NotEq(current[0].Content, edit.Files[0].Content, "Files[0].Content unchanged, want modified content from the response")
	ck.NotEq("", edit.Rationale, "Rationale empty")

	ck.Require().Len(sender.lastReq, 1, "requests sent = %d, want 1", len(sender.lastReq))
	if sender.lastReq[0].ForceTool != "propose_skill_edit" {
		t.Errorf("request did not force propose_skill_edit tool choice: %q", sender.lastReq[0].ForceTool)
	}

	// The user turn must have surfaced the existing file content so the
	// model could edit it minimally.
	userMsg := sender.lastReq[0].Messages[len(sender.lastReq[0].Messages)-1]
	var sawCurrentContent bool
	for _, block := range userMsg.Content {
		if tx, ok := block.(msg.Text); ok && strings.Contains(tx.Text, "old trigger") {
			sawCurrentContent = true
		}
	}
	ck.True(sawCurrentContent, "request did not include the current file's content")
}

const newSkillDraftInput = `{
	"files": [{"path": "skills/sc-diagnose-replication-lag/SKILL.md", "content": "---\nname: sc-diagnose-replication-lag\ndescription: new skill\n---\n\nbody"}],
	"rationale": "no existing skill covered this, so created one"
}`

func TestDraftNewSkillPathInstructsSkillsDirLayout(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondDraftToolUse(t, newSkillDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	f := fixtureRankedFinding()
	edit, err := Draft(context.Background(), c, f, nil, p, "brent", nil)
	ck.Require().NoError(err, "Draft")
	ck.Eq(f.Title, edit.FindingTitle, "FindingTitle")
	ck.Require().Len(edit.Files, 1, "Files")
	if !strings.HasPrefix(edit.Files[0].Path, "skills/") || !strings.HasSuffix(edit.Files[0].Path, "SKILL.md") {
		t.Errorf("Files[0].Path = %q, want under skills/<name>/SKILL.md", edit.Files[0].Path)
	}

	userMsg := sender.lastReq[0].Messages[len(sender.lastReq[0].Messages)-1]
	var sawNewSkillInstruction bool
	for _, block := range userMsg.Content {
		if tx, ok := block.(msg.Text); ok && strings.Contains(tx.Text, "new skill") &&
			strings.Contains(tx.Text, "skills/<skill-name>/SKILL.md") {
			sawNewSkillInstruction = true
		}
	}
	ck.True(sawNewSkillInstruction, "request did not instruct the new-skill skills/<name>/SKILL.md layout")
}

func TestDraftRetriesOnceOnMalformedResponse(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondTextOnly("forgot to call the tool"),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Require().NoError(err, "Draft")
	ck.NotEmpty(edit.Files, "Files empty after retry")

	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry)", len(sender.lastReq))

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Eq(msg.User, lastMsg.Role, "last message in retry request is role")
	var found bool
	for _, block := range lastMsg.Content {
		if tx, ok := block.(msg.Text); ok && strings.Contains(tx.Text, "no propose_skill_edit tool_use block") {
			found = true
		}
	}
	ck.True(found, "retry request's last user turn did not contain the parse error; content=%+v", lastMsg.Content)
}

func TestDraftFailsAfterTwoMalformedResponses(t *testing.T) {
	ck := assert.NewAborting(t)
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondTextOnly("first malformed reply"),
		respondTextOnly("second malformed reply"),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Error(err, "Draft: want error after two malformed responses, got nil")
	ck.Len(sender.lastReq, 2, "requests sent = %d, want 2 (initial + one retry, no third attempt)", len(sender.lastReq))
}

func TestDraftRejectsAbsolutePathAndRetries(t *testing.T) {
	ck := assert.NewCollecting(t)
	invalidInput := `{
		"files": [{"path": "/etc/skills/SKILL.md", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Require().NoError(err, "Draft")
	ck.NotEmpty(edit.Files, "Files empty after retry")
	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry after invalid path)", len(sender.lastReq))

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Eq(msg.User, lastMsg.Role, "last message in retry request is role")
	// The first response DID call propose_skill_edit (with invalid input),
	// so the retry must answer that dangling tool_use with a tool_result —
	// a plain user-text turn here is what the real Anthropic API rejects.
	ck.Require().Len(lastMsg.Content, 1, "retry request's last user turn = %+v, want a single tool_result block", lastMsg.Content)
	tr, ok := lastMsg.Content[0].(msg.ToolResult)
	ck.Require().True(ok, "retry request's last user turn = %+v, want a tool_result block", lastMsg.Content)
	ck.Eq("tu_1", tr.ToolUseID, "tool_result.tool_use_id")
	ck.True(tr.IsError, "tool_result.is_error = false, want true")
	var found bool
	for _, sub := range tr.Content {
		if tx, ok := sub.(msg.Text); ok && strings.Contains(tx.Text, "must be relative") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request did not surface the absolute-path validation error; tool_result content=%+v", tr.Content)
	}
}

func TestDraftRejectsBackslashPathAndRetries(t *testing.T) {
	ck := assert.NewCollecting(t)
	invalidInput := `{
		"files": [{"path": "..\\\\..\\\\etc\\\\passwd", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, editDraftInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	edit, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Require().NoError(err, "Draft")
	ck.NotEmpty(edit.Files, "Files empty after retry")
	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (one retry after backslash path)", len(sender.lastReq))

	retryReq := sender.lastReq[1]
	lastMsg := retryReq.Messages[len(retryReq.Messages)-1]
	ck.Require().Len(lastMsg.Content, 1, "retry request's last user turn = %+v, want a single tool_result block", lastMsg.Content)
	tr, ok := lastMsg.Content[0].(msg.ToolResult)
	ck.Require().True(ok, "retry request's last user turn = %+v, want a tool_result block", lastMsg.Content)
	var found bool
	for _, sub := range tr.Content {
		if tx, ok := sub.(msg.Text); ok && strings.Contains(tx.Text, "\\\\") {
			found = true
		}
	}
	if !found {
		t.Errorf("retry request did not surface the backslash-path validation error; tool_result content=%+v", tr.Content)
	}
}

func TestDraftRejectsNormalizedTraversalPathAndFailsAfterRetry(t *testing.T) {
	ck := assert.NewAborting(t)
	// "a/../../b" contains two literal ".." segments; the segment scan
	// rejects it outright regardless of what it would normalize to.
	invalidInput := `{
		"files": [{"path": "a/../../b", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, invalidInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Error(err, "Draft: want error, got nil")
	ck.Len(sender.lastReq, 2, "requests sent = %d, want 2 (initial + one retry)", len(sender.lastReq))
}

func TestDraftRejectsDotDotPathAndFailsAfterRetry(t *testing.T) {
	ck := assert.NewCollecting(t)
	invalidInput := `{
		"files": [{"path": "skills/../../etc/passwd", "content": "some content"}],
		"rationale": "oops"
	}`
	sender := &fakeSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondDraftToolUse(t, invalidInput),
		respondDraftToolUse(t, invalidInput),
	}}
	c := testDetectClient(t, sender)
	p := &Profile{DraftModel: "claude-haiku-4-5"}

	_, err := Draft(context.Background(), c, fixtureRankedFinding(), nil, p, "brent", nil)
	ck.Require().Error(err, "Draft: want error, got nil")
	ck.StrContains(err.Error(), "..", "error = %v, want mention of \"..\" segment rejection", err)
	ck.Require().Len(sender.lastReq, 2, "requests sent = %d, want 2 (initial + one retry)", len(sender.lastReq))
}
