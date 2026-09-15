// SPDX-License-Identifier: Apache-2.0

package daraja

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// The coordination prompt merges into the wire params only when the child
// gets the MCP agent-control surface, and the caller's own appendix rides the
// same field — the flag is last-wins, so the merge must be one text, never a
// second element. This pins the gate half; the byte-identity of the two launch
// paths that consume this mapping is pinned by test/integration's
// TestClaudeArgvIdenticalAcrossPaths.
func TestClaudeParamsForRequestCoordinationPrompt(t *testing.T) {
	req := protocol.SpawnRequest{AppendSystemPrompt: "be terse"}

	got := ClaudeParamsForRequest(req, true)
	if got.AppendSystemPrompt != claudeargv.CoordinationPrompt+"\n\nbe terse" {
		t.Errorf("mcpAgentControl=true: AppendSystemPrompt = %q, want prompt + caller text", got.AppendSystemPrompt)
	}

	got = ClaudeParamsForRequest(req, false)
	if got.AppendSystemPrompt != "be terse" {
		t.Errorf("mcpAgentControl=false: AppendSystemPrompt = %q, want the caller's text untouched", got.AppendSystemPrompt)
	}

	got = ClaudeParamsForRequest(protocol.SpawnRequest{}, true)
	if !strings.Contains(got.AppendSystemPrompt, "agent_spawn") {
		t.Errorf("mcpAgentControl=true, no caller prompt: AppendSystemPrompt = %q, want the coordination prompt", got.AppendSystemPrompt)
	}
}
