// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end tests for an MCP server hosted by an EXECUTOR: a real daemon, a
// real `rafiki executor serve` process started with --mcp-command, a real
// stdio MCP server (test/integration/fixtures/mcpstdio), and a real fundi child
// driven by the hidden --fake-turns sender.
//
// What only this file can prove:
//   - the argv an operator declared on the executor is what actually ran, and
//     the child's model can CALL the tool it exposes;
//   - a via_executor entry resolves ONCE at spawn, so a config that cannot be
//     served refuses the spawn by name instead of leaving a toolless child;
//   - when the hosted process dies mid-call, its stderr reaches the model in
//     the failed call (and the call is not silently retried into a second
//     crash — D9's rule).
//
// Same rules as the other MCP integration tests: -count=1 always (go test
// caching cannot see through to a daemon subprocess).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// buildMCPFixture builds the stdio MCP server fixture and returns its path, so
// an executor's --mcp-command can name a real binary.
func buildMCPFixture(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mcpstdio")
	cmd := exec.Command("go", "build", "-o", out, "./fixtures/mcpstdio")
	cmd.Dir = "."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mcp fixture: %v\n%s", err, b)
	}
	return out
}

// writeViaExecutorMCPConfig writes an mcp.json whose single server lives on an
// executor, and returns its path (a path on the DAEMON's machine, which in this
// harness is the same host).
func writeViaExecutorMCPConfig(t *testing.T, selector, command string) string {
	t.Helper()
	c := assert.NewAborting(t)
	body, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"fixture": map[string]any{
				"via_executor": map[string]any{"selector": selector, "command": command},
			},
		},
	})
	c.NoError(err, "marshal mcp.json")
	path := filepath.Join(t.TempDir(), "mcp.json")
	c.NoError(os.WriteFile(path, body, 0o600), "write mcp.json")
	return path
}

// fakeTurnsMCPCall scripts two assistant turns: a call to the executor-hosted
// MCP tool, then a plain end_turn reply.
func fakeTurnsMCPCall(t *testing.T, toolName string, input map[string]any) string {
	t.Helper()
	c := assert.NewAborting(t)
	inputJSON, err := json.Marshal(input)
	c.NoError(err, "marshal tool input")
	toolUse := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use",` +
		`"content":[{"type":"tool_use","id":"tu_1","name":"` + toolName + `","input":` + string(inputJSON) + `}],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}`
	const endTurn = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":4,"output_tokens":2}}`
	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(toolUse+"\n"+endTurn+"\n"), 0o600), "write fake-turns script")
	return path
}

// TestIntegration_MCPViaExecutor_ChildCallsAHostedTool is the whole feature:
// an operator declares an MCP server on the executor, the cluster's mcp.json
// points at that declaration, and a child of the daemon calls the tool — the
// process itself running on the executor, under its environment.
func TestIntegration_MCPViaExecutor_ChildCallsAHostedTool(t *testing.T) {
	c := assert.NewAborting(t)
	g := bootGrantDaemon(t, requireExecutorDB(t))

	fixture := buildMCPFixture(t)
	g.enrollExecutorExtra(t, map[string]string{"env": "home"},
		"--mcp-command", "fixture="+fixture)
	g.waitForLiveExecutors(t, 1)

	mcpPath := writeViaExecutorMCPConfig(t, "env=home", "fixture")
	script := fakeTurnsMCPCall(t, "mcp__fixture__echo", map[string]any{"message": "hello"})

	client := g.control(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:              t.TempDir(),
		Kind:             protocol.KindFundi,
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		McpConfig:        mcpPath,
		ExtraArgs:        []string{"--fake-turns", script},
	}))
	c.Require().NoError(err, "spawn with a via_executor MCP server")
	childID := resp.Msg.GetChildId()
	c.NotEq("", childID, "spawn returned no child id")

	es := openChildEventsAll(t, g, childID)
	pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer pcancel()
	_, err = client.SendFrame(pctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"go"}`,
	}))
	c.Require().NoError(err, "prompt the child")

	// The tool result is what the MODEL saw: it can only be there if the
	// daemon's session reached the process the executor spawned.
	es.waitEvent(t, userTextContains(childID, "echo: hello"), 30*time.Second)
}

// A via_executor entry whose selector matches no live executor REFUSES the
// spawn, by name, instead of quietly leaving the child toolless.
func TestIntegration_MCPViaExecutor_SpawnRefusedWhenNoExecutorMatches(t *testing.T) {
	c := assert.NewAborting(t)
	g := bootGrantDaemon(t, requireExecutorDB(t))

	g.enrollExecutorExtra(t, map[string]string{"env": "home"},
		"--mcp-command", "fixture="+buildMCPFixture(t))
	g.waitForLiveExecutors(t, 1)

	mcpPath := writeViaExecutorMCPConfig(t, "env=nowhere", "fixture")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := g.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:              t.TempDir(),
		Kind:             protocol.KindFundi,
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		McpConfig:        mcpPath,
	}))
	c.Require().Error(err, "spawn succeeded with an unresolvable via_executor server")
	c.StrContains(err.Error(), "mcp server", "error")
	c.StrContains(err.Error(), "env=nowhere", "error names the selector")
}

// An executor that is live and matches the selector but declares no such MCP
// command says so — the laptop has not been updated, and that is the message
// the operator needs.
func TestIntegration_MCPViaExecutor_SpawnRefusedWhenNameUndeclared(t *testing.T) {
	c := assert.NewAborting(t)
	g := bootGrantDaemon(t, requireExecutorDB(t))

	g.enrollExecutorExtra(t, map[string]string{"env": "home"},
		"--mcp-command", "other="+buildMCPFixture(t))
	g.waitForLiveExecutors(t, 1)

	mcpPath := writeViaExecutorMCPConfig(t, "env=home", "fixture")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := g.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:              t.TempDir(),
		Kind:             protocol.KindFundi,
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		McpConfig:        mcpPath,
	}))
	c.Require().Error(err, "spawn succeeded with an undeclared MCP command")
	c.StrContains(err.Error(), "declares MCP command", "error")
	c.StrContains(err.Error(), "--mcp-command fixture=", "error names the flag")
}

// When the hosted process dies mid-call, its stderr reaches the MODEL in the
// failed call — and the call is not retried into a second crash.
func TestIntegration_MCPViaExecutor_HostedCrashSurfacesItsStderr(t *testing.T) {
	c := assert.NewAborting(t)
	g := bootGrantDaemon(t, requireExecutorDB(t))

	fixture := buildMCPFixture(t)
	g.enrollExecutorExtra(t, map[string]string{"env": "home"},
		"--mcp-command", "fixture="+fixture+" -crash-on-call")
	g.waitForLiveExecutors(t, 1)

	mcpPath := writeViaExecutorMCPConfig(t, "env=home", "fixture")
	script := fakeTurnsMCPCall(t, "mcp__fixture__echo", map[string]any{"message": "hello"})

	client := g.control(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:              t.TempDir(),
		Kind:             protocol.KindFundi,
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		McpConfig:        mcpPath,
		ExtraArgs:        []string{"--fake-turns", script},
	}))
	c.Require().NoError(err, "spawn with a crashing via_executor MCP server")
	childID := resp.Msg.GetChildId()

	es := openChildEventsAll(t, g, childID)
	pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer pcancel()
	_, err = client.SendFrame(pctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"go"}`,
	}))
	c.Require().NoError(err, "prompt the child")

	// The tool call fails; the model sees WHY, because the executor's stderr
	// tail rides the exit frame into the transport's error.
	ev := es.waitEvent(t, userTextContains(childID, "credentials expired"), 30*time.Second)
	c.NotNil(ev, "no event carried the crashed server's stderr")
}
