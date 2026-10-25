package child

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// A main-thread tool start carries no parent_tool_use_id. A sub-agent's frames
// produce nothing on the parent (TestNativeIgnoresSubagentFrames), so the only
// start a parent publishes is its own.
func TestClaudeNativeToolStartCarriesNoParentToolUseID(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	main := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_main","name":"bash","input":{}}]}}`)

	var starts []*rafikiv1.ToolExecutionStart
	for _, ev := range p.BusFramesNative(main, 1000) {
		if ts := ev.GetToolExecutionStart(); ts != nil {
			starts = append(starts, ts)
		}
	}

	c.Len(starts, 1, "main-thread frame tool starts = %d, want 1", len(starts))
	c.Eq("tu_main", starts[0].GetToolUseId(), "tool_use_id")
	c.Eq("", starts[0].GetParentToolUseId(), "parent_tool_use_id on a main-thread frame")
}
