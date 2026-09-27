// SPDX-License-Identifier: Apache-2.0

package daraja

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// The coordination prompt merges into the wire params only when the child
// gets the MCP agent-control surface, and the caller's own appendix rides the
// same field — the flag is last-wins, so the merge must be one text, never a
// second element. This pins the gate half; the byte-identity of the two launch
// paths that consume this mapping is pinned by test/integration's
// TestClaudeArgvIdenticalAcrossPaths.
func TestClaudeParamsForRequestCoordinationPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{AppendSystemPrompt: "be terse"}

	got := ClaudeParamsForRequest(req, true)
	c.Eq(claudeargv.CoordinationPrompt+"\n\nbe terse", got.AppendSystemPrompt, "mcpAgentControl=true: AppendSystemPrompt")

	got = ClaudeParamsForRequest(req, false)
	c.Eq("be terse", got.AppendSystemPrompt, "mcpAgentControl=false: AppendSystemPrompt")

	got = ClaudeParamsForRequest(protocol.SpawnRequest{}, true)
	c.StrContains(got.AppendSystemPrompt, "agent_spawn", "mcpAgentControl=true, no caller prompt: AppendSystemPrompt")
}
