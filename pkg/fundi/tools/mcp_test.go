package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/multigres/testkit/assert"
)

// addArgs is the input type for the in-memory test server's "add" tool.
type addArgs struct {
	A int `json:"a" jsonschema:"first addend"`
	B int `json:"b" jsonschema:"second addend"`
}

// newTestMCPServer builds an in-process mcp.Server exposing three tools used
// across the tests below: "add" (normal success), "list-items" (a hyphenated
// name, to exercise normalization), and "fail" (always returns an error, to
// exercise the IsError -> Go error path).
func newTestMCPServer(name string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: name}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "add", Description: "add two integers"},
		func(_ context.Context, _ *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d", args.A+args.B)}},
			}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "list-items", Description: "hyphenated tool name"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "fail", Description: "always fails"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("boom")
		})

	return server
}

// newCustomMCPServer builds an in-process mcp.Server named serverName
// exposing one echo-style tool per name in toolNames: each returns "ok:
// <name>" as text when called. Used by tests that need specific (often
// deliberately odd, e.g. containing dots/spaces/overlong) tool names rather
// than the fixed set in newTestMCPServer.
func newCustomMCPServer(serverName string, toolNames ...string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName}, nil)
	for _, name := range toolNames {
		name := name
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: "test tool " + name},
			func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + name}},
				}, nil, nil
			})
	}
	return server
}

// newBigOutputMCPServer builds an in-process mcp.Server named serverName
// exposing a single tool, toolName, that returns output (expected to be
// large) as text - used to exercise OutputPolicy.Clip's spill path.
func newBigOutputMCPServer(serverName, toolName, output string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: toolName, Description: "returns a large fixed output"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: output}},
			}, nil, nil
		})
	return server
}

// inMemoryRef connects an in-process server the way connectInMemory did, but
// wraps it in the redialable mcpServerSession, whose dial creates a FRESH
// transport pair (and a fresh client) on every call — the in-process analogue
// of a server that restarted and accepts a new initialize between calls.
// The session is closed automatically via t.Cleanup.
func inMemoryRef(t *testing.T, server *mcp.Server) *mcpServerSession {
	t.Helper()
	ctx := context.Background()
	ref := &mcpServerSession{name: "test-server", dial: func(ctx context.Context) (*mcp.ClientSession, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			return nil, err
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		return client.Connect(ctx, clientTransport, nil)
	}}
	sess, err := ref.dial(ctx)
	assert.NewAborting(t).NoError(err, "dial")
	ref.sess = sess
	t.Cleanup(ref.close)
	return ref
}

// TestRegisterMCPServerTools covers the core registration + dispatch path:
// tools from an MCP session appear in Definitions() under
// mcp__<server>__<tool> and Execute round-trips a real call through the
// protocol.
func TestRegisterMCPServerTools(t *testing.T) {
	c := assert.NewCollecting(t)
	session := inMemoryRef(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	c.Require().NoError(registerMCPServerTools(context.Background(), r, "my-server", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	names := map[string]bool{}
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}
	for _, want := range []string{"mcp__my_server__add", "mcp__my_server__list_items", "mcp__my_server__fail"} {
		c.False(!names[want], "expected tool %q to be registered, got %v", want, names)
	}

	out, err := r.Execute(context.Background(), "mcp__my_server__add", json.RawMessage(`{"a":2,"b":3}`))
	c.Require().NoError(err, "unexpected error")
	c.Require().Eq("5", out, "expected \"5\", got")
}

// TestRegisterMCPServerToolsNormalizesHyphens covers the stated requirement
// that hyphens in both server and tool names are normalized to underscores
// in the registered tool name (Anthropic tool names reject dots and, more to
// the point here, this project's dispatch logic pattern-matches on the
// underscore form).
func TestRegisterMCPServerToolsNormalizesHyphens(t *testing.T) {
	c := assert.NewAborting(t)
	session := inMemoryRef(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "my-cool-server", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	found := false
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__my_cool_server__list_items" {
			found = true
		}
	}
	c.True(found, "expected hyphens in server and tool name to be normalized to underscores")
}

// TestRegisterMCPServerToolsIsErrorBecomesGoError covers the stated
// requirement that a CallToolResult with IsError set is surfaced as a Go
// error (so agentloop marks it an is_error tool result the model can react
// to), not swallowed or returned as ordinary success text.
func TestRegisterMCPServerToolsIsErrorBecomesGoError(t *testing.T) {
	c := assert.NewAborting(t)
	session := inMemoryRef(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	_, err := r.Execute(context.Background(), "mcp__srv__fail", json.RawMessage(`{}`))
	c.Error(err, "expected an error for a tool result with IsError set")
	c.StrContains(err.Error(), "boom", "expected error to mention the tool's failure text, got %v", err)
}

// TestRegisterMCPServerToolsInputSchemaPassedThrough covers the requirement
// that ListTools input schemas pass through verbatim into
// anthropic.ToolInputSchemaParam, rather than being narrowed or dropped.
func TestRegisterMCPServerToolsInputSchemaPassedThrough(t *testing.T) {
	c := assert.NewAborting(t)
	session := inMemoryRef(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	var propsJSON []byte
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__srv__add" {
			b, err := json.Marshal(def.OfTool.InputSchema.Properties)
			c.NoError(err, "marshal properties")
			propsJSON = b
		}
	}
	c.NotNil(propsJSON, "mcp__srv__add not found")
	for _, want := range []string{`"a"`, `"b"`} {
		c.StrContains(string(propsJSON), want, "expected input schema properties to contain %s, got %s", want, propsJSON)
	}
}

// TestLoadMCPConfig covers parsing both server shapes .mcp.json supports:
// stdio (command/args/env) and HTTP (url/headers).
func TestLoadMCPConfig(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	content := `{
		"mcpServers": {
			"stdio-server": {"command": "myserver", "args": ["--flag"], "env": {"FOO": "bar"}},
			"http-server": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer xyz"}}
		}
	}`
	c.NoError(os.WriteFile(path, []byte(content), 0o644))

	cfg, err := LoadMCPConfig(path)
	c.NoError(err, "LoadMCPConfig")
	c.Len(cfg.MCPServers, 2, "expected 2 servers, got %d", len(cfg.MCPServers))

	stdio, ok := cfg.MCPServers["stdio-server"]
	c.True(ok, "expected stdio-server in config")
	c.False(stdio.Command != "myserver" || len(stdio.Args) != 1 || stdio.Args[0] != "--flag" || stdio.Env["FOO"] != "bar", "unexpected stdio server config: %+v", stdio)

	httpSrv, ok := cfg.MCPServers["http-server"]
	c.True(ok, "expected http-server in config")
	c.False(httpSrv.URL != "https://example.com/mcp" || httpSrv.Headers["Authorization"] != "Bearer xyz", "unexpected http server config: %+v", httpSrv)
}

// TestLoadMCPConfigMissingFile covers the returned-error path for a config
// file that doesn't exist.
func TestLoadMCPConfigMissingFile(t *testing.T) {
	_, err := LoadMCPConfig(filepath.Join(t.TempDir(), "nope.json"))
	assert.NewAborting(t).Error(err, "expected an error for a missing config file")
}

// TestConnectMCPSkipsServerThatFailsToConnect covers the stated resilience
// requirement: a server that fails to connect (here, a nonexistent command)
// is logged and skipped rather than making ConnectMCP fail outright.
func TestConnectMCPSkipsServerThatFailsToConnect(t *testing.T) {
	c := assert.NewAborting(t)
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"bad": {Command: "definitely-not-a-real-command-xyz-fundi-test"},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{})
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.Empty(r.Definitions(), "expected no tools registered from a failing server, got")
}

// TestConnectMCPSkipsServerWithNoCommandOrURL covers a malformed config
// entry (neither command nor url set) being skipped the same way a
// connection failure is, rather than panicking or propagating an error that
// would take down every other configured server.
func TestConnectMCPSkipsServerWithNoCommandOrURL(t *testing.T) {
	c := assert.NewAborting(t)
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{"empty": {}}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{})
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.Empty(r.Definitions(), "expected no tools registered, got")
}

// anthropicToolNameRETest mirrors the exact grammar the Anthropic API
// enforces for tool names, kept independent of anthropicToolNameRE in
// mcp.go so this test can't be trivially satisfied by weakening the
// production regexp.
var anthropicToolNameRETest = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// TestRegisterMCPServerToolsNormalizesDotsAndOtherSeparators covers the T13
// correctness fix: a real MCP tool name containing a dot (very common, e.g.
// "github.create_issue") or a space must normalize to an all-underscores
// name that satisfies Anthropic's ^[a-zA-Z0-9_-]{1,128}$ grammar and remain
// callable under that name. Before this fix, normalizeMCPName replaced only
// hyphens, so a dotted name produced an INVALID tool name that would 400
// the entire tools array on the next turn.
func TestRegisterMCPServerToolsNormalizesDotsAndOtherSeparators(t *testing.T) {
	c := assert.NewAborting(t)
	const oddName = "github.create issue"
	session := inMemoryRef(t, newCustomMCPServer("test-server", oddName))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	const want = "mcp__srv__github_create_issue"
	var found bool
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool registered as %q, got %v", want, r.Definitions())
	}
	c.True(anthropicToolNameRETest.MatchString(want), "registered name %q does not match Anthropic's tool name grammar", want)

	out, err := r.Execute(context.Background(), want, json.RawMessage(`{}`))
	c.NoError(err, "unexpected error calling %q", want)
	c.Eq("ok:"+oddName, out, "expected")
}

// TestRegisterMCPServerToolsSkipsOverlongName covers the requirement that a
// built mcp__server__tool name failing Anthropic's grammar even AFTER
// normalization (here: length > 128) is skipped with a warning rather than
// registered invalid - and that the rest of the same server's tools are
// unaffected.
func TestRegisterMCPServerToolsSkipsOverlongName(t *testing.T) {
	c := assert.NewAborting(t)
	longName := strings.Repeat("a", 130)
	session := inMemoryRef(t, newCustomMCPServer("test-server", longName, "short"))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	names := map[string]bool{}
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}
	for name := range names {
		c.LessOrEqual(128, len(name), "expected no registered name over 128 characters, got %q (%d chars)", name, len(name))
	}
	c.False(!names["mcp__srv__short"], "expected the other tool on the same server to still be registered, got %v", names)
	c.Len(names, 1, "expected exactly one registered tool (the overlong one skipped), got")
}

// TestRegisterMCPServerToolsSkipsCollidingNormalizedNames covers the
// collision-handling requirement: two distinct tool names that normalize to
// the same mcp__server__tool string must not both register - the later one
// is skipped (with a warning) rather than silently shadowing the first via
// Registry.Register's overwrite semantics. Only one registration must
// result, and neither call may panic.
func TestRegisterMCPServerToolsSkipsCollidingNormalizedNames(t *testing.T) {
	c := assert.NewAborting(t)
	session := inMemoryRef(t, newCustomMCPServer("test-server", "list-items", "list_items"))

	r := NewRegistry()
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	count := 0
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__srv__list_items" {
			count++
		}
	}
	c.Eq(1, count, "expected exactly one registration of the colliding name, got %d (defs: %v)", count, r.Definitions())
}

// TestRegisterMCPServerToolsClipsOversizedOutput covers the requirement
// that MCP tool results go through OutputPolicy.Clip like every other
// ToolFunc in this package: an over-budget result is clipped for the model,
// with the FULL result spilled to SpillDir (mirrors bash_test.go's
// TestBashOutputGoesThroughSpillPolicy).
func TestRegisterMCPServerToolsClipsOversizedOutput(t *testing.T) {
	c := assert.NewAborting(t)
	spillDir := t.TempDir()
	full := strings.Repeat("x", 2000)
	session := inMemoryRef(t, newBigOutputMCPServer("test-server", "big", full))

	r := NewRegistry()
	p := OutputPolicy{Budget: 200, SpillDir: spillDir}
	c.NoError(registerMCPServerTools(context.Background(), r, "srv", session, p, make(map[string]string)), "registerMCPServerTools")

	out, err := r.Execute(context.Background(), "mcp__srv__big", json.RawMessage(`{}`))
	c.NoError(err, "unexpected error")
	c.LessOrEqual(400, len(out), "expected clipped output, got")
	c.StrContains(out, "elided", "expected elision marker, got")

	entries, err := os.ReadDir(spillDir)
	c.NoError(err)
	c.Len(entries, 1, "expected exactly one spill file, got %d", len(entries))
	spilled, err := os.ReadFile(filepath.Join(spillDir, entries[0].Name()))
	c.NoError(err)
	c.Eq(full, string(spilled), "spilled file does not hold the full output: got %d bytes, want %d", len(spilled), len(full))
}

// TestMCPSessionRecoversAfterServerSideDeath covers the resilience contract
// that motivated mcpServerSession: an MCP server MAY terminate a session at
// any time (pod restart, rmcp session eviction after the SSE stream drops),
// after which the go-sdk marks the client connection terminal and every
// CallTool on it would fail forever. The next call after the server-side
// death must redial through the same config and succeed, with tool
// registration untouched.
func TestMCPSessionRecoversAfterServerSideDeath(t *testing.T) {
	c := assert.NewAborting(t)
	server := newTestMCPServer("test-server")
	ctx := context.Background()

	// Same dial shape as inMemoryRef, plus a record of the CURRENT
	// connection's server-side session so the test can kill it — the
	// in-process analogue of the server dropping the session.
	var (
		mu   sync.Mutex
		last *mcp.ServerSession
	)
	ref := &mcpServerSession{name: "test-server"}
	ref.dial = func(ctx context.Context) (*mcp.ClientSession, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ssess, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		last = ssess
		mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		return client.Connect(ctx, clientTransport, nil)
	}
	sess, err := ref.dial(ctx)
	c.NoError(err, "initial dial")
	ref.sess = sess
	t.Cleanup(ref.close)

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", ref, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	out, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":2,"b":3}`))
	c.False(err != nil || out != "5", "pre-kill call: err=%v out=%q", err, out)

	mu.Lock()
	ss := last
	mu.Unlock()
	c.NotNil(ss, "no server-side session was recorded")
	c.NoError(ss.Close(), "server-side close")

	out, err = r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":20,"b":22}`))
	c.NoError(err, "post-kill call should recover via redial")
	c.Eq("42", out, "post-kill call: expected")
}

// TestMCPSessionRedialFailureIsReported covers the failure side: when the
// redial itself fails (server down and staying down), the tool call surfaces
// the dial error rather than hanging or panicking, and a call inside the
// cooldown window fails fast without re-attempting the dial.
func TestMCPSessionRedialFailureIsReported(t *testing.T) {
	c := assert.NewAborting(t)
	session := inMemoryRef(t, newTestMCPServer("test-server"))
	ctx := context.Background()

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", session, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	session.dial = func(_ context.Context) (*mcp.ClientSession, error) {
		return nil, errors.New("dial boom")
	}
	session.close() // drop the live session: the next call must redial

	_, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.False(err == nil || !strings.Contains(err.Error(), "dial boom"), "expected the redial failure to surface, got %v", err)

	_, err = r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.False(err == nil || !strings.Contains(err.Error(), "unreachable"), "expected a cooldown-throttled failure, got %v", err)
}
