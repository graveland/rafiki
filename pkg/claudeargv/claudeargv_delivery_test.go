// SPDX-License-Identifier: Apache-2.0

package claudeargv

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

// Headless claude must always be launched with --replay-user-messages: it is
// the only signal that a message reached a turn, and the daemon's delivery
// receipt is built from it. The flag is emitted by the ONE shared builder, so
// every launch path — the local subprocess, daraja's respawn, an executor's
// StagedArgv — gets it without each remembering to add it.
func TestClaudeDeliveryReplayFlagIsInEveryHeadlessArgv(t *testing.T) {
	c := assert.NewCollecting(t)
	_, proxied := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	c.Require().NotEq("", proxied.MCPConfig, "fixture: ClaudeEnv produced no MCP config for a proxied session")

	for name, p := range map[string]Params{
		"zero":            {},
		"model":           {Model: "claude-opus-5"},
		"model args":      {ModelArgs: []string{"--model", "glm-5.2"}},
		"resume":          {ResumeSession: "sess-abc"},
		"inline mcp":      {MCPConfig: `{"mcpServers":{}}`},
		"staged mcp":      {MCPConfigFile: "/state/prompts/abc.json"},
		"permission mode": {PermissionMode: "acceptEdits"},
		"extra args":      {ExtraArgs: []string{"--foo", "bar"}},
		"proxied spawn":   ParamsFromSpawnRequest(protocol.SpawnRequest{Model: "glm-5.2"}, proxied),
	} {
		argv := Build(p)
		n := 0
		for _, a := range argv {
			if a == ReplayUserMessages {
				n++
			}
		}
		c.Eq(1, n, "%s: argv %v: want exactly one %s, got", name, argv, ReplayUserMessages)
	}
}

// The flag hard-requires the stream-json pair on BOTH directions, so it must
// ride in the same argv as both format flags — never in one that lacks them.
func TestClaudeDeliveryReplayFlagRidesTheStreamJSONPair(t *testing.T) {
	c := assert.NewCollecting(t)
	argv := Build(Params{})
	for _, want := range []string{"--input-format", "--output-format", ReplayUserMessages} {
		c.Contains(argv, want, "argv %v: missing %q, so the replay flag would be refused at launch; got", argv, want)
	}
}

// Interactive `rafiki claude` keeps the TTY and never gets the flag: claude
// rejects it without stream-json input and output, which interactive mode does
// not select.
func TestClaudeDeliveryReplayFlagNeverReachesInteractiveArgv(t *testing.T) {
	c := assert.NewCollecting(t)
	_, proxied := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	for name, p := range map[string]Params{
		"zero":           {Mode: ModeInteractive},
		"model":          {Mode: ModeInteractive, Model: "claude-opus-5"},
		"proxied":        {Mode: ModeInteractive, MCPConfig: proxied.MCPConfig, ModelArgs: proxied.ModelArgs},
		"with user args": {Mode: ModeInteractive, UserArgs: []string{"--resume", "abc-123"}},
	} {
		argv := Build(p)
		c.NotContains(argv, ReplayUserMessages, "%s: interactive argv %v carries the headless replay flag; got", name, argv)
		c.NotContains(argv, "--input-format", "%s: interactive argv %v selects stream-json input; got", name, argv)
	}
}
