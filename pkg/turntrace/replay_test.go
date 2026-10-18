// SPDX-License-Identifier: Apache-2.0

package turntrace

import (
	"testing"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestClaudeReplayShape drives the observer with the exact event sequence
// pkg/child/provider_claude_state.go emits for one claude turn with a Task
// sub-agent: the assistant message declares a tool call before its execution
// start, a sub-agent's tool call names its Task call as parent_tool_use_id,
// and each tool's result arrives as a user message after its execution end.
func TestClaudeReplayShape(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)

	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "task1", "Task", `{"description":"explore"}`))
	o.Observe("c1", evToolStart(tsAt(2), "task1", "Task", ""))
	o.Observe("c1", evAssistantToolUse(tsAt(3), "t2", "Bash", `{"command":"ls"}`))
	o.Observe("c1", evToolStart(tsAt(4), "t2", "Bash", "task1"))
	o.Observe("c1", evToolEnd(tsAt(5), "t2", false))
	o.Observe("c1", evToolResult(tsAt(6), "t2", "file.txt", false))
	o.Observe("c1", evToolEnd(tsAt(7), "task1", false))
	o.Observe("c1", evToolResult(tsAt(8), "task1", "explored", false))
	o.Observe("c1", evAssistantText(tsAt(9), "all done"))
	o.Observe("c1", evTurnEnd(tsAt(10), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	spans := rec.Ended()
	turn := findOne(t, spans, "invoke_agent c1")
	task := findOne(t, spans, "execute_tool Task")
	sub := findOne(t, spans, "execute_tool Bash")

	c := assert.NewAborting(t)
	c.Eq(3, len(spans), "span count")
	c.Eq(false, turn.Parent().IsValid(), "turn is a root")
	c.Eq(turn.SpanContext().SpanID(), task.Parent().SpanID(), "Task nests under the turn")
	c.Eq(task.SpanContext().SpanID(), sub.Parent().SpanID(), "sub-agent tool nests under Task")
	c.Eq("all done", attrsOf(turn)["rafiki.reply_snippet"].AsString(), "reply snippet")
}
