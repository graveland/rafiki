package tools

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// These are prose assertions, not behavior — but the rule is checkable
// (does the text say what we mean it to), so it is checked rather than left
// to a human proofreading a multi-line Go string constant. See
// docs/plans/2026-09-02-agent-cost-guardrails-design.md's coordinator-
// notification sections for why this text changed.
func TestAgentSpawnDescriptionDoesNotInviteWatchingForCompletion(t *testing.T) {
	c := assert.NewCollecting(t)
	d := agentSpawnDescription
	c.NotStrContains(d, "you can watch it with agent_list and agent_view", "agent_spawn description still invites polling agent_list/agent_view to watch for completion")
	c.False(!strings.Contains(d, "do not") && !strings.Contains(d, "Do not"), "agent_spawn description does not explicitly tell the model not to poll")
	c.False(!strings.Contains(d, "notified") && !strings.Contains(d, "told when"), "agent_spawn description does not explain that settlement is a notification, not something to check for")
}

func TestAgentListDescriptionDiscouragesLooping(t *testing.T) {
	d := agentListDescription
	assert.NewCollecting(t).False(!strings.Contains(d, "loop") && !strings.Contains(d, "poll"), "agent_list description does not warn against calling it in a loop")
}

func TestAgentViewDescriptionDiscouragesLooping(t *testing.T) {
	d := agentViewDescription
	assert.NewCollecting(t).False(!strings.Contains(d, "loop") && !strings.Contains(d, "poll"), "agent_view description does not warn against calling it in a loop")
}
