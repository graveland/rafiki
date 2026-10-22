// SPDX-License-Identifier: Apache-2.0

package execpool

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/executor"

	"github.com/multigres/testkit/assert"
)

// mcpPoolFixture stands up a real pkg/executor.Server hosting the given MCP
// commands, joins it to a fresh Pool, and returns the pool and its executor ID.
// Both halves of the relay are the real thing, so what is exercised here is the
// wire between them.
func mcpPoolFixture(t *testing.T, commands map[string][]string) (*Pool, string) {
	t.Helper()
	const executorID = "exec-mcp"

	p := New(newFakeStore(executorID))
	p.healthInterval = time.Hour // no health polling noise in this test

	srv := executor.NewServer(executor.Options{Root: t.TempDir(), NoLSP: true, MCPCommands: commands})
	assert.NewAborting(t).NoError(joinViaUpgrade(t, p, srv))

	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })
	return p, executorID
}

// A whole MCP session runs through the relay: the handshake, tools/list and a
// call, with the executor relaying BYTES and never parsing the protocol.
func TestMCPStdioTransportRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	p, executorID := mcpPoolFixture(t, map[string][]string{"fixture": mcpFixtureArgv("-mcpstdio")})

	tr := NewMCPStdioTransport(p, executorID, "fixture")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(context.Background(), tr, nil)
	c.Require().NoError(err, "Connect")
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	c.Require().NoError(err, "ListTools")
	c.Eq(len(tools.Tools), 1, "tool count")
	c.Eq(tools.Tools[0].Name, "echo", "tool name")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello"},
	})
	c.Require().NoError(err, "CallTool")
	c.Require().True(len(res.Content) == 1, "content blocks: %d", len(res.Content))
	text, ok := res.Content[0].(*mcp.TextContent)
	c.Require().True(ok, "content is not text")
	c.Eq(text.Text, "echo: hello", "tool result")

	_, _, exited := tr.Exited()
	c.False(exited, "the transport recorded an exit while the process is alive")
}

// An observed process exit becomes a TAIL-BEARING error, and is recorded on the
// transport — the two facts D9's no-retry rule is built on.
func TestMCPStdioTransportExitCarriesTheStderrTail(t *testing.T) {
	c := assert.NewCollecting(t)
	p, executorID := mcpPoolFixture(t, map[string][]string{"flapper": mcpFixtureArgv("-mcpstdio-crash")})

	tr := NewMCPStdioTransport(p, executorID, "flapper")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	_, err := client.Connect(context.Background(), tr, nil)
	c.Require().Error(err, "connected to a server that dies on startup")

	status, tail, exited := tr.Exited()
	c.True(exited, "the transport did not record the observed exit")
	c.Eq(status, "exit code 3", "status")
	c.StrContains(tail, "fatal: credentials expired", "tail")
}

// An UNDECLARED command fails the dial synchronously, naming what the executor
// does declare: the caller learns before it begins its handshake.
func TestMCPStdioTransportUndeclaredNameFailsTheDial(t *testing.T) {
	c := assert.NewCollecting(t)
	p, executorID := mcpPoolFixture(t, map[string][]string{"declared": mcpFixtureArgv("-mcpstdio")})

	tr := NewMCPStdioTransport(p, executorID, "hesabu")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	_, err := client.Connect(context.Background(), tr, nil)
	c.Require().Error(err, "connected to an undeclared command")
	c.StrContains(err.Error(), `"hesabu"`, "error names the server")
	c.StrContains(err.Error(), "declared", "error names what the executor declares")
}

// A nil pool is a closed door, never a silent local spawn.
func TestMCPStdioTransportNilPoolFailsClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := NewMCPStdioTransport(nil, "exec-mcp", "hesabu")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	_, err := client.Connect(context.Background(), tr, nil)
	c.Require().Error(err, "connected with no pool")
	c.StrContains(err.Error(), "no executor pool", "error")
}

// An executor too old to implement MCPStdio is a VERSION GAP, named as such:
// chasing a "broken MCP server" config is the wrong repair for a machine that
// predates the RPC.
func TestMCPStdioTransportReportsAVersionGap(t *testing.T) {
	c := assert.NewCollecting(t)
	const executorID = "exec-old"
	p := New(newFakeStore(executorID))
	p.healthInterval = time.Hour
	// stubHandler embeds UnimplementedExecutorServiceHandler, so MCPStdio
	// answers CodeUnimplemented — exactly what an older binary does.
	c.NoError(joinViaUpgrade(t, p, &stubHandler{executorID: executorID}), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	tr := NewMCPStdioTransport(p, executorID, "hesabu")
	_, err := tr.Connect(context.Background())
	c.Require().Error(err, "connected to an executor without MCPStdio")
	c.StrContains(err.Error(), "does not implement MCPStdio", "error")
	c.StrContains(err.Error(), "older than this daemon", "error names the cause")
	c.StrContains(err.Error(), executorID, "error names the executor")
}
