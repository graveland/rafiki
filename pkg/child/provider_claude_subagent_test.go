package child

import "testing"

func TestSubagentFrameReportsTheSpawningToolCall(t *testing.T) {
	p := newClaudeProvider()
	subagent := []byte(`{"type":"assistant","message":{"id":"msg_sub1","role":"assistant",` +
		`"content":[{"type":"text","text":"working"}]},"parent_tool_use_id":"toolu_ABC"}`)
	got, ok := p.SubagentFrame(subagent)
	if !ok {
		t.Fatal("a frame with parent_tool_use_id must be reported")
	}
	if got.MessageID != "msg_sub1" {
		t.Errorf("MessageID = %q, want %q", got.MessageID, "msg_sub1")
	}
	if got.ParentToolUseID != "toolu_ABC" {
		t.Errorf("ParentToolUseID = %q, want %q", got.ParentToolUseID, "toolu_ABC")
	}
}

func TestMainThreadFramesAreNotReported(t *testing.T) {
	p := newClaudeProvider()
	// parent_tool_use_id is null on the main thread: see
	// pkg/child/testdata/claude/startup_and_turn.jsonl.
	main := []byte(`{"type":"assistant","message":{"id":"msg_main","role":"assistant",` +
		`"content":[{"type":"text","text":"hi"}]},"parent_tool_use_id":null}`)
	if _, ok := p.SubagentFrame(main); ok {
		t.Error("a main-thread frame must not be reported as a subagent frame")
	}
	if _, ok := p.SubagentFrame([]byte(`not json`)); ok {
		t.Error("malformed input must not be reported")
	}
	noID := []byte(`{"type":"assistant","message":{"role":"assistant"},"parent_tool_use_id":"toolu_X"}`)
	if _, ok := p.SubagentFrame(noID); ok {
		t.Error("a frame with no message id cannot be joined and must not be reported")
	}
}
