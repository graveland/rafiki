package child

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// A sub-agent's assistant frame names the Task tool call that spawned it in
// parent_tool_use_id. ToolExecutionStart must carry that id so downstream spans
// can nest a sub-agent's tool call under the Task call, not under the main
// thread; a main-thread frame (no parent_tool_use_id) yields the empty string.
func TestClaudeNativeToolStartCarriesParentToolUseID(t *testing.T) {
	c := assert.NewAborting(t)

	toolStarts := func(p *claudeProvider, line []byte) []*rafikiv1.ToolExecutionStart {
		var out []*rafikiv1.ToolExecutionStart
		for _, ev := range p.BusFramesNative(line, 1000) {
			if ts := ev.GetToolExecutionStart(); ts != nil {
				out = append(out, ts)
			}
		}
		return out
	}

	p := newClaudeProvider()
	sub := []byte(`{"type":"assistant","parent_tool_use_id":"toolu_task1","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_sub","name":"bash","input":{}}]}}`)
	starts := toolStarts(p, sub)
	c.Len(starts, 1, "sub-agent frame tool starts = %d, want 1", len(starts))
	c.Eq("tu_sub", starts[0].GetToolUseId(), "tool_use_id")
	c.Eq("toolu_task1", starts[0].GetParentToolUseId(), "parent_tool_use_id")

	main := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_main","name":"bash","input":{}}]}}`)
	starts = toolStarts(p, main)
	c.Len(starts, 1, "main-thread frame tool starts = %d, want 1", len(starts))
	c.Eq("tu_main", starts[0].GetToolUseId(), "tool_use_id")
	c.Eq("", starts[0].GetParentToolUseId(), "parent_tool_use_id on a main-thread frame")
}
