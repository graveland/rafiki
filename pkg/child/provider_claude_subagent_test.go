package child

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestSubagentFrameReportsTheSpawningToolCall(t *testing.T) {
	c := assert.NewCollecting(t)
	p := newClaudeProvider()
	subagent := []byte(`{"type":"assistant","message":{"id":"msg_sub1","role":"assistant",` +
		`"content":[{"type":"text","text":"working"}]},"parent_tool_use_id":"toolu_ABC"}`)
	got, ok := p.SubagentFrame(subagent)
	c.Require().True(ok, "a frame with parent_tool_use_id must be reported")
	c.Eq("msg_sub1", got.MessageID, "MessageID")
	c.Eq("toolu_ABC", got.ParentToolUseID, "ParentToolUseID")
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
	_, ok := p.SubagentFrame(noID)
	assert.NewCollecting(t).False(ok, "a frame with no message id cannot be joined and must not be reported")
}
