package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

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
	ref := &mcpServerSession{name: "test-server", dial: func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			return nil, nil, err
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		sess, err := client.Connect(ctx, clientTransport, nil)
		return sess, nil, err
	}}
	sess, _, err := ref.dial(ctx)
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
		names[def.Name] = true
	}
	for _, want := range []string{"mcp__my_server__add", "mcp__my_server__list_items", "mcp__my_server__fail"} {
		c.False(!names[want], "expected tool %q to be registered, got %v", want, names)
	}

	outRes, err := r.Execute(context.Background(), "mcp__my_server__add", json.RawMessage(`{"a":2,"b":3}`))
	out := outRes.Text
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
		if def.Name == "mcp__my_cool_server__list_items" {
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
		if def.Name == "mcp__srv__add" {
			propsJSON = def.InputSchema
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
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, nil)
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
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, nil)
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
		if def.Name == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool registered as %q, got %v", want, r.Definitions())
	}
	c.True(anthropicToolNameRETest.MatchString(want), "registered name %q does not match Anthropic's tool name grammar", want)

	outRes, err := r.Execute(context.Background(), want, json.RawMessage(`{}`))
	out := outRes.Text
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
		names[def.Name] = true
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
		if def.Name == "mcp__srv__list_items" {
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

	outRes, err := r.Execute(context.Background(), "mcp__srv__big", json.RawMessage(`{}`))
	out := outRes.Text
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
	ref.dial = func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ssess, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		mu.Lock()
		last = ssess
		mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		sess, err := client.Connect(ctx, clientTransport, nil)
		return sess, nil, err
	}
	sess, _, err := ref.dial(ctx)
	c.NoError(err, "initial dial")
	ref.sess = sess
	t.Cleanup(ref.close)

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", ref, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	outRes, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":2,"b":3}`))
	out := outRes.Text
	c.False(err != nil || out != "5", "pre-kill call: err=%v out=%q", err, out)

	mu.Lock()
	ss := last
	mu.Unlock()
	c.NotNil(ss, "no server-side session was recorded")
	c.NoError(ss.Close(), "server-side close")

	outRes, err = r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":20,"b":22}`))
	out = outRes.Text
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

	session.dial = func(_ context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		return nil, nil, errors.New("dial boom")
	}
	session.close() // drop the live session: the next call must redial

	_, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.False(err == nil || !strings.Contains(err.Error(), "dial boom"), "expected the redial failure to surface, got %v", err)

	_, err = r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.False(err == nil || !strings.Contains(err.Error(), "unreachable"), "expected a cooldown-throttled failure, got %v", err)
}

// A via_executor entry is refused BY NAME at load time: the argv and the
// environment live on the executor, and a config error that degrades into "no
// tools" is invisible.
func TestLoadMCPConfigRefusesBadViaExecutor(t *testing.T) {
	cases := []struct{ name, server, body, want string }{
		{
			name:   "alongside an entry command",
			server: "hesabu",
			body:   `{"mcpServers":{"hesabu":{"command":"hesabu","args":["mcp"],"via_executor":{"selector":"name=x","command":"hesabu"}}}}`,
			want:   "cannot be combined",
		},
		{
			name:   "alongside an entry url",
			server: "codescan",
			body:   `{"mcpServers":{"codescan":{"url":"http://x","via_executor":{"selector":"name=x","proxy":"codescan"}}}}`,
			want:   "cannot be combined",
		},
		{
			name:   "neither command nor proxy",
			server: "hesabu",
			body:   `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=x"}}}}`,
			want:   "exactly one of command",
		},
		{
			name:   "both command and proxy",
			server: "hesabu",
			body:   `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=x","command":"hesabu","proxy":"p"}}}}`,
			want:   "both command and proxy",
		},
		{
			name:   "empty selector",
			server: "hesabu",
			body:   `{"mcpServers":{"hesabu":{"via_executor":{"selector":"","command":"hesabu"}}}}`,
			want:   "needs a selector",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			path := filepath.Join(t.TempDir(), "mcp.json")
			c.Require().NoError(os.WriteFile(path, []byte(tc.body), 0o600), "write config")
			_, err := LoadMCPConfig(path)
			c.Require().Error(err, "accepted %s", tc.name)
			c.StrContains(err.Error(), `server "`+tc.server+`"`, "error names the server")
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}

func TestLoadMCPConfigAcceptsViaExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	body := `{"mcpServers":{
		"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}},
		"codescan":{"via_executor":{"selector":"name=greyshift","proxy":"codescan"}},
		"local":{"command":"hesabu","args":["mcp"]}
	}}`
	c.Require().NoError(os.WriteFile(path, []byte(body), 0o600), "write config")
	cfg, err := LoadMCPConfig(path)
	c.Require().NoError(err, "LoadMCPConfig")
	c.EqDeep(cfg.ViaExecutorNames(), []string{"codescan", "hesabu"}, "via_executor names")
	c.Eq(cfg.MCPServers["hesabu"].ViaExecutor.Command, "hesabu", "command")
	c.Eq(cfg.MCPServers["codescan"].ViaExecutor.Proxy, "codescan", "proxy")
}

// A via_executor server with no dialer fails rather than falling through to a
// direct dial of the daemon's own localhost: this is what `rafikid agent`
// prints, since it has no executor pool.
func TestDialMCPServerWithoutADialerFailsClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	sc := MCPServerConfig{ViaExecutor: &MCPViaExecutor{Selector: "name=x", Command: "hesabu"}}
	_, _, err := dialMCPServer(context.Background(), "hesabu", sc, nil, nil, nil)
	c.Require().Error(err, "a via_executor server dialled with no dialer")
	c.StrContains(err.Error(), "via_executor requires the executor plane", "error")
	c.StrContains(err.Error(), "RAFIKI_EXECUTORS_ENABLED=1", "error names the knob")
}

// Keepalive is set for BOTH transports: it is the only thing that sees a
// wedged-but-alive server holding its pipe.
func TestMCPClientOptionsCarryKeepAlive(t *testing.T) {
	c := assert.NewCollecting(t)
	opts := mcpClientOptions()
	c.Eq(opts.KeepAlive, mcpKeepAlive, "KeepAlive")
	c.Eq(opts.KeepAliveFailureThreshold, mcpKeepAliveFailures, "KeepAliveFailureThreshold")
}

// fakeMCPHealth is a transport's exit record, as execpool's MCPStdioTransport
// reports it.
type fakeMCPHealth struct {
	exited bool
	status string
	tail   string
}

func (h *fakeMCPHealth) Exited() (string, string, bool) { return h.status, h.tail, h.exited }

// sequencedHealth answers "not exited" to the FIRST Exited() call and "exited"
// to every one after it. call() reads the record once before the call and once
// after it, so this models — deterministically — an exit that appeared while
// the call was in flight, which is the case the no-retry rule is about.
type sequencedHealth struct {
	mu    sync.Mutex
	calls int
}

func (h *sequencedHealth) Exited() (string, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return "exit code 1", "fatal: credentials expired", h.calls > 1
}

// An OBSERVED process exit is not retried — and the rule must still hold when
// the SDK has wrapped the underlying error, which is why it reads a health flag
// rather than an error value.
func TestMCPCallDoesNotRetryAnObservedExit(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	server := newTestMCPServer("test-server")

	var (
		mu    sync.Mutex
		last  *mcp.ServerSession
		dials int
		// The exit appears only from the second Exited() read, i.e. while the
		// call is in flight.
		health = &sequencedHealth{}
		ref    = &mcpServerSession{name: "test-server"}
	)
	ref.dial = func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ssess, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		mu.Lock()
		last = ssess
		mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		sess, err := client.Connect(ctx, clientTransport, nil)
		return sess, health, err
	}
	sess, h, err := ref.dial(ctx)
	c.NoError(err, "initial dial")
	ref.sess, ref.health = sess, h
	t.Cleanup(ref.close)

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", ref, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	_ = last
	// The client's session is finished, so the call fails the way a dead session
	// does; health reports the observed exit only from the second read on, i.e.
	// while that call is in flight.
	c.NoError(sess.Close(), "client-side close")

	_, err = r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.Require().Error(err, "the call succeeded against a dead server")
	c.StrContains(err.Error(), "not retried", "error")
	c.StrContains(err.Error(), "exit code 1", "error carries the exit status")
	c.StrContains(err.Error(), "fatal: credentials expired", "error carries the stderr tail")
	mu.Lock()
	got := dials
	mu.Unlock()
	c.Eq(got, 1, "an observed exit was retried")
}

// An AMBIGUOUS death — a dropped session with no observed process exit — is
// still retried once, which is the behaviour that keeps a restarted server's
// tools working for the life of an agent.
func TestMCPCallRetriesAnAmbiguousDeath(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	server := newTestMCPServer("test-server")

	var (
		mu    sync.Mutex
		last  *mcp.ServerSession
		dials int
		ref   = &mcpServerSession{name: "test-server"}
	)
	ref.dial = func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ssess, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		mu.Lock()
		last = ssess
		mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		sess, err := client.Connect(ctx, clientTransport, nil)
		return sess, &fakeMCPHealth{}, err
	}
	sess, h, err := ref.dial(ctx)
	c.NoError(err, "initial dial")
	ref.sess, ref.health = sess, h
	t.Cleanup(ref.close)

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", ref, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	mu.Lock()
	ss := last
	mu.Unlock()
	c.NoError(ss.Close(), "server-side close")

	res, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.NoError(err, "an ambiguous death was not retried")
	c.Eq(res.Text, "3", "result after redial")
	mu.Lock()
	got := dials
	mu.Unlock()
	c.Eq(got, 2, "dial count")
}

// A server that died while its session sat IDLE is not this call's failure: the
// exit record was already there before the call began, so the call must redial
// rather than refuse (the agent never learns of an idle death — the design's
// §3 rule). Only an exit observed DURING the call suppresses the retry.
func TestMCPCallRetriesAfterAnIdleExit(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	server := newTestMCPServer("test-server")

	var (
		mu    sync.Mutex
		dials int
		ref   = &mcpServerSession{name: "test-server"}
		// The FIRST transport's record: the process died with no call in
		// flight. The redial gets a fresh, healthy one.
		health = &fakeMCPHealth{exited: true, status: "exit code 1", tail: "restarting"}
	)
	ref.dial = func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
		mu.Lock()
		dials++
		h := health
		mu.Unlock()
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			return nil, nil, err
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
		sess, err := client.Connect(ctx, clientTransport, nil)
		return sess, h, err
	}
	sess, h, err := ref.dial(ctx)
	c.NoError(err, "initial dial")
	ref.sess, ref.health = sess, h
	t.Cleanup(ref.close)

	r := NewRegistry()
	c.NoError(registerMCPServerTools(ctx, r, "srv", ref, OutputPolicy{}, make(map[string]string)), "registerMCPServerTools")

	// The session is finished and the transport already records the exit: the
	// idle death case.
	c.NoError(sess.Close(), "client-side close")
	mu.Lock()
	health = &fakeMCPHealth{} // the redial's session is healthy
	mu.Unlock()

	res, err := r.Execute(ctx, "mcp__srv__add", json.RawMessage(`{"a":1,"b":2}`))
	c.NoError(err, "an idle exit was treated as an in-flight crash")
	c.Eq(res.Text, "3", "result after redial")
	mu.Lock()
	got := dials
	mu.Unlock()
	c.Eq(got, 2, "dial count")
}

// ─── direct-URL OAuth factory (RuntimeOptions.MCPHTTP) ──────────────────────
//
// These tests assert on the HTTP requests a REAL SDK session sends to a fixture
// server, not on a struct assignment: the whole seam exists to change what
// leaves the process, so a test that only reads the transport's fields would
// pass with the headers going anywhere.

// mcpFixtureRequest is one HTTP request that reached a fixture server.
type mcpFixtureRequest struct {
	Method string
	Path   string
	Header http.Header
}

// mcpOAuthFixture is a real streamable-HTTP MCP server (the "add"/"list-items"/
// "fail" tool set) that records every request it receives. wraps are applied
// INSIDE the recorder, so a wrapper can refuse (401), redirect, or fail a
// session without hiding the request from the assertions.
type mcpOAuthFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	reqs   []mcpFixtureRequest
}

func newMCPOAuthFixture(t *testing.T, wraps ...func(http.Handler) http.Handler) *mcpOAuthFixture {
	t.Helper()
	f := &mcpOAuthFixture{}
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return newTestMCPServer("fixture") },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)
	var h http.Handler = handler
	for _, wrap := range wraps {
		h = wrap(h)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *mcpOAuthFixture) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, mcpFixtureRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
}

// endpoint is the configured URL a fixture serves MCP from.
func (f *mcpOAuthFixture) endpoint() string { return f.server.URL + "/mcp" }

// requests returns a copy of everything received so far.
func (f *mcpOAuthFixture) requests() []mcpFixtureRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpFixtureRequest(nil), f.reqs...)
}

// headerOn reports the LAST value seen on path for name, and whether the header
// was present at all — presence is distinct from emptiness, which is exactly
// the distinction the Authorization precedence rule turns on.
func (f *mcpOAuthFixture) headerOn(path, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].Path != path {
			continue
		}
		vs, ok := f.reqs[i].Header[http.CanonicalHeaderKey(name)]
		if !ok || len(vs) == 0 {
			return "", false
		}
		return vs[len(vs)-1], true
	}
	return "", false
}

// testOAuthHandler is a PASSIVE auth.OAuthHandler, the shape the daemon's
// factory returns: a token source (nil when the owner has no credential, which
// is how a public server keeps working) and an Authorize that never prompts.
type testOAuthHandler struct {
	token     string
	authorize error
	tokenErr  error

	mu         sync.Mutex
	authorized int
}

func (h *testOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	if h.tokenErr != nil {
		return nil, h.tokenErr
	}
	if h.token == "" {
		return nil, nil
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: h.token}), nil
}

func (h *testOAuthHandler) Authorize(context.Context, *http.Request, *http.Response) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authorized++
	if h.authorize != nil {
		return h.authorize
	}
	return errors.New("mcp: a login is required")
}

func (h *testOAuthHandler) authorizeCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authorized
}

// recordingMCPHTTPFactory records every factory call and answers with whatever
// the test set. It doubles as the "was the factory consulted at all?" probe.
type recordingMCPHTTPFactory struct {
	handler auth.OAuthHandler
	client  *http.Client
	err     error

	mu    sync.Mutex
	calls []mcpFactoryCall
}

type mcpFactoryCall struct {
	server   string
	endpoint string
	headers  map[string]string
}

func (f *recordingMCPHTTPFactory) factory(_ context.Context, server, endpoint string, headers map[string]string) (auth.OAuthHandler, *http.Client, error) {
	f.mu.Lock()
	f.calls = append(f.calls, mcpFactoryCall{server: server, endpoint: endpoint, headers: headers})
	f.mu.Unlock()
	return f.handler, f.client, f.err
}

func (f *recordingMCPHTTPFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *recordingMCPHTTPFactory) snapshot() []mcpFactoryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpFactoryCall(nil), f.calls...)
}

// A case-insensitive Authorization key on the entry — including an EMPTY value
// — means the entry carries its own auth: the factory is never consulted, and
// the header the operator wrote is what reaches the endpoint.
func TestMcpOAuthAuthorizationHeaderSkipsTheFactory(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"canonical", "Authorization", "Bearer explicit"},
		{"lowercase", "authorization", "Bearer explicit"},
		{"uppercase", "AUTHORIZATION", "Bearer explicit"},
		{"mixed", "AuThOrIzAtIoN", "Bearer explicit"},
		{"empty value", "Authorization", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			fixture := newMCPOAuthFixture(t)
			factory := &recordingMCPHTTPFactory{err: errors.New("the factory must not be called")}
			cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
				"srv": {URL: fixture.endpoint(), Headers: map[string]string{tc.key: tc.value}},
			}}

			r := NewRegistry()
			shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
			c.NoError(err, "ConnectMCP")
			defer shutdown()

			c.Eq(0, factory.count(), "factory calls for an entry with an explicit Authorization header")
			c.False(len(r.Definitions()) == 0, "no tool registered: %+v", r.Definitions())

			got, present := fixture.headerOn("/mcp", "Authorization")
			c.True(present, "the configured Authorization header never reached the endpoint: %+v", fixture.requests())
			c.Eq(tc.value, got, "Authorization on the endpoint")
		})
	}
}

// Unrelated configured headers coexist with OAuth: they do not disable it, and
// the bearer the handler supplies rides alongside them on the same request.
func TestMcpOAuthUnrelatedHeadersCoexistWithTheBearer(t *testing.T) {
	c := assert.NewAborting(t)
	fixture := newMCPOAuthFixture(t)
	handler := &testOAuthHandler{token: "tok-passive"}
	// The factory's client is the GUARDED one: its transport must stay underneath
	// the header wrapper, or the destination policy would be silently replaced by
	// DefaultTransport while the bearer still rode along.
	guarded := &countingTransport{base: http.DefaultTransport}
	factory := &recordingMCPHTTPFactory{handler: handler, client: &http.Client{Transport: guarded}}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint(), Headers: map[string]string{"X-Mcp-Tenant": "acme"}},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	calls := factory.snapshot()
	c.Eq(1, len(calls), "factory calls")
	c.Eq("srv", calls[0].server, "factory server name")
	c.Eq(fixture.endpoint(), calls[0].endpoint, "factory endpoint")
	c.Eq("acme", calls[0].headers["X-Mcp-Tenant"], "factory headers")

	tenant, present := fixture.headerOn("/mcp", "X-Mcp-Tenant")
	c.True(present, "an unrelated configured header disabled OAuth and was itself dropped: %+v", fixture.requests())
	c.Eq("acme", tenant, "X-Mcp-Tenant on the endpoint")
	bearer, present := fixture.headerOn("/mcp", "Authorization")
	c.True(present, "no bearer reached the endpoint with OAuth enabled: %+v", fixture.requests())
	c.Eq("Bearer tok-passive", bearer, "Authorization on the endpoint")

	// Every request went THROUGH the guarded transport the factory supplied.
	c.True(guarded.requests() > 0, "the factory's guarded transport was replaced as the base")
}

// countingTransport wraps a real transport and counts what reached it, standing
// in for the guarded client a factory returns.
type countingTransport struct {
	base http.RoundTripper

	mu sync.Mutex
	n  int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

func (t *countingTransport) requests() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// A passive factory with NO stored credential still dials: the handler returns a
// nil token source, no Authorization is sent, and a public server works. This is
// what keeps OAuth from turning every direct-URL entry into a login-required one.
func TestMcpOAuthPublicServerWorksWithoutACredential(t *testing.T) {
	c := assert.NewAborting(t)
	fixture := newMCPOAuthFixture(t)
	handler := &testOAuthHandler{}
	factory := &recordingMCPHTTPFactory{handler: handler, client: &http.Client{}}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint()},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.Eq(1, factory.count(), "factory calls")
	c.False(len(r.Definitions()) == 0, "no tool registered from a public server: %+v", r.Definitions())
	if _, present := fixture.headerOn("/mcp", "Authorization"); present {
		t.Fatalf("an Authorization header was sent with no stored credential: %+v", fixture.requests())
	}
	c.Eq(0, handler.authorizeCalls(), "Authorize calls on a server that accepted the request")
}

// A 401 with no usable credential surfaces as the NAMED login hint on the
// existing skip-log path: that server is logged and skipped, the child keeps its
// other tools, and ConnectMCP itself never fails.
func TestMcpOAuth401CarriesTheNamedLoginHintThroughTheSkipLog(t *testing.T) {
	c := assert.NewAborting(t)
	fixture := newMCPOAuthFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	})
	const hint = "server srv needs a login: run `rafiki mcp login srv`"
	handler := &testOAuthHandler{authorize: errors.New(hint)}
	factory := &recordingMCPHTTPFactory{handler: handler, client: &http.Client{}}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint()},
	}}

	logs := captureSlog(t)
	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
	c.NoError(err, "a server needing a login must not fail the whole MCP connect")
	defer shutdown()

	c.Empty(r.Definitions(), "tools registered from a server that refused every request")
	// The count is the SDK's own: a 401 during initialize is rejected and
	// reported, and nothing here re-dials. What matters is that our layer never
	// turns a refused request into an interactive prompt or a second dial.
	c.True(handler.authorizeCalls() >= 1, "Authorize was never consulted for a 401: %d", handler.authorizeCalls())
	c.StrContains(logs.String(), "failed to connect to server, skipping", "skip log line")
	c.StrContains(logs.String(), hint, "the skip log must carry the named login hint")
	c.StrContains(logs.String(), "srv", "the skip log must name the server")
}

// An unreadable credential REFUSES that server without attempting an anonymous
// dial: "the credential store is down" must never be answered as "this server is
// public".
func TestMcpOAuthCredentialLookupFailureRefusesTheServer(t *testing.T) {
	c := assert.NewAborting(t)
	fixture := newMCPOAuthFixture(t)
	factory := &recordingMCPHTTPFactory{err: errors.New("mcpoauth: database is unavailable")}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint(), Headers: map[string]string{"X-Mcp-Tenant": "acme"}},
	}}

	logs := captureSlog(t)
	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.Eq(1, factory.count(), "factory calls")
	c.Eq(0, len(fixture.requests()), "requests reaching the server after a credential lookup failure (an anonymous retry would send one): %+v", fixture.requests())
	c.Empty(r.Definitions(), "tools registered despite the refused credential")
	c.StrContains(logs.String(), "mcpoauth: database is unavailable", "the lookup failure is named in the skip log")
}

// The factory belongs to DIRECT-URL entries only: a stdio entry's argv and a
// via_executor entry's synthetic transport are decided elsewhere, and looking
// up an owner-bound credential for either would be a credential applied to a
// machine the entry does not name.
func TestMcpOAuthFactoryIsNotCalledForStdioOrExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	factory := &recordingMCPHTTPFactory{err: errors.New("the factory must not be called")}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"stdio":  {Command: "definitely-not-a-real-command-xyz-fundi-test"},
		"remote": {ViaExecutor: &MCPViaExecutor{Selector: "name=greyshift", Command: "srv"}},
	}}
	dial := func(ctx context.Context, name string, _ MCPServerConfig) (mcp.Transport, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		if _, err := newTestMCPServer(name).Connect(ctx, serverTransport, nil); err != nil {
			return nil, err
		}
		return clientTransport, nil
	}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, dial, factory.factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.Eq(0, factory.count(), "factory calls for stdio/via_executor entries")
	c.False(len(r.Definitions()) == 0, "the via_executor server's tools were not registered: %+v", r.Definitions())
}

// Configured headers reach exactly the configured endpoint: scheme, host (port
// included), path and query. A redirect hop gets neither the configured headers
// nor the Authorization the SDK itself stamps on the request — including the
// same-HOSTNAME, different-port hop Go's own redirect logic would happily carry
// an Authorization token across.
func TestMcpOAuthConfiguredHeadersStayOnTheExactEndpoint(t *testing.T) {
	c := assert.NewAborting(t)
	const tenant = "Bearer configured-secret"

	var evilMu sync.Mutex
	var evil []mcpFixtureRequest
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilMu.Lock()
		evil = append(evil, mcpFixtureRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
		evilMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evilSrv.Close)

	fixture := newMCPOAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				http.Redirect(w, r, evilSrv.URL+"/mcp", http.StatusTemporaryRedirect)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	_, client, err := mcpHTTPClient(context.Background(), "srv", MCPServerConfig{
		URL:     fixture.endpoint(),
		Headers: map[string]string{"Authorization": tenant},
	}, nil)
	c.NoError(err, "mcpHTTPClient")

	// 1. The configured endpoint itself: the header is attached, and an
	//    Authorization set independently of the config (how the SDK's OAuth
	//    handler stamps a request) survives.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, fixture.endpoint(), nil)
	c.NoError(err, "new request")
	req.Header.Set("Authorization", "Bearer sdk-bearer")
	resp, err := client.Do(req)
	c.NoError(err, "request to the configured endpoint")
	if resp != nil {
		resp.Body.Close()
	}
	got, present := fixture.headerOn("/mcp", "Authorization")
	c.True(present, "no Authorization on the configured endpoint")
	c.Eq(tenant, got, "the configured header must win on its own endpoint")

	// 2. Same origin, a DIFFERENT path: not the configured endpoint, so the
	//    configured header is not attached.
	req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, fixture.server.URL+"/other", nil)
	c.NoError(err, "new request")
	resp, err = client.Do(req)
	c.NoError(err, "request to another path on the same origin")
	if resp != nil {
		resp.Body.Close()
	}
	if _, present := fixture.headerOn("/other", "Authorization"); present {
		t.Fatalf("the configured header followed the client to another path: %+v", fixture.requests())
	}

	// 3. A redirect to the same hostname on another port: Go copies
	//    Authorization across that hop (it compares hostnames), so only this
	//    transport's containment keeps the credential from following.
	req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, fixture.server.URL+"/redirect", nil)
	c.NoError(err, "new request")
	req.Header.Set("Authorization", "Bearer sdk-bearer")
	resp, err = client.Do(req)
	c.NoError(err, "redirecting request")
	if resp != nil {
		resp.Body.Close()
	}
	evilMu.Lock()
	received := append([]mcpFixtureRequest(nil), evil...)
	evilMu.Unlock()
	c.Eq(1, len(received), "the redirect target did not receive the hop: %+v", received)
	if len(received) == 1 {
		if vs, ok := received[0].Header["Authorization"]; ok {
			t.Fatalf("Authorization followed a redirect to another origin: %v", vs)
		}
	}
}

// A reconnected session goes back through the factory: the redial closure
// carries it, so the replacement session is dialled with the same owner-bound
// credential and the same endpoint containment, not anonymously.
func TestMcpOAuthFactoryIsSuppliedOnRedial(t *testing.T) {
	c := assert.NewAborting(t)
	var once sync.Once
	fixture := newMCPOAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading request body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if r.Method == http.MethodPost && strings.Contains(string(body), "tools/call") {
				failed := false
				once.Do(func() { failed = true })
				if failed {
					// The server terminated the session: the SDK maps this to
					// ErrSessionMissing, which call() recovers from by redialing.
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	factory := &recordingMCPHTTPFactory{
		handler: &testOAuthHandler{token: "tok-redial"},
		client:  &http.Client{},
	}
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint()},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, factory.factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	res, err := r.Execute(context.Background(), "mcp__srv__add", json.RawMessage(`{"a": 2, "b": 3}`))
	c.NoError(err, "a dead session should have been redialed through the factory")
	c.Eq("5", res.Text, "tool result after redial")
	c.Eq(2, factory.count(), "factory calls: one for the dial, one for the redial")

	for _, req := range fixture.requests() {
		if req.Method != http.MethodPost {
			continue
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok-redial" {
			t.Fatalf("a request after a redial lost the owner-bound bearer: %q (%s %s)", got, req.Method, req.Path)
		}
	}
}

// Without the option, a direct-URL entry behaves exactly as it did before the
// seam: configured headers are sent, nothing is looked up, and the server works.
func TestMcpOAuthWithoutAFactoryPreservesStandaloneBehaviour(t *testing.T) {
	c := assert.NewAborting(t)
	fixture := newMCPOAuthFixture(t)
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"srv": {URL: fixture.endpoint(), Headers: map[string]string{"X-Mcp-Tenant": "acme"}},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{}, nil, nil, nil)
	c.NoError(err, "ConnectMCP")
	defer shutdown()

	c.False(len(r.Definitions()) == 0, "no tool registered: %+v", r.Definitions())
	got, present := fixture.headerOn("/mcp", "X-Mcp-Tenant")
	c.True(present, "the configured header was not sent without a factory: %+v", fixture.requests())
	c.Eq("acme", got, "X-Mcp-Tenant on the endpoint")
}
