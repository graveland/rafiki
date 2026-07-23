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
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// connectInMemory connects a fresh client session to server over an
// in-memory transport pair, mirroring what ConnectMCP does for a real
// stdio/HTTP transport. The session is closed automatically via t.Cleanup.
func connectInMemory(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Logf("session.Close: %v", err)
		}
	})
	return session
}

// TestRegisterMCPServerTools covers the core registration + dispatch path:
// tools from an MCP session appear in Definitions() under
// mcp__<server>__<tool> and Execute round-trips a real call through the
// protocol.
func TestRegisterMCPServerTools(t *testing.T) {
	session := connectInMemory(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "my-server", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	names := map[string]bool{}
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}
	for _, want := range []string{"mcp__my_server__add", "mcp__my_server__list_items", "mcp__my_server__fail"} {
		if !names[want] {
			t.Errorf("expected tool %q to be registered, got %v", want, names)
		}
	}

	out, err := r.Execute(context.Background(), "mcp__my_server__add", json.RawMessage(`{"a":2,"b":3}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "5" {
		t.Fatalf("expected \"5\", got %q", out)
	}
}

// TestRegisterMCPServerToolsNormalizesHyphens covers the stated requirement
// that hyphens in both server and tool names are normalized to underscores
// in the registered tool name (Anthropic tool names reject dots and, more to
// the point here, this project's dispatch logic pattern-matches on the
// underscore form).
func TestRegisterMCPServerToolsNormalizesHyphens(t *testing.T) {
	session := connectInMemory(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "my-cool-server", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	found := false
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__my_cool_server__list_items" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected hyphens in server and tool name to be normalized to underscores")
	}
}

// TestRegisterMCPServerToolsIsErrorBecomesGoError covers the stated
// requirement that a CallToolResult with IsError set is surfaced as a Go
// error (so agentloop marks it an is_error tool result the model can react
// to), not swallowed or returned as ordinary success text.
func TestRegisterMCPServerToolsIsErrorBecomesGoError(t *testing.T) {
	session := connectInMemory(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	_, err := r.Execute(context.Background(), "mcp__srv__fail", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for a tool result with IsError set")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error to mention the tool's failure text, got %v", err)
	}
}

// TestRegisterMCPServerToolsInputSchemaPassedThrough covers the requirement
// that ListTools input schemas pass through verbatim into
// anthropic.ToolInputSchemaParam, rather than being narrowed or dropped.
func TestRegisterMCPServerToolsInputSchemaPassedThrough(t *testing.T) {
	session := connectInMemory(t, newTestMCPServer("test-server"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	var propsJSON []byte
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__srv__add" {
			b, err := json.Marshal(def.OfTool.InputSchema.Properties)
			if err != nil {
				t.Fatalf("marshal properties: %v", err)
			}
			propsJSON = b
		}
	}
	if propsJSON == nil {
		t.Fatal("mcp__srv__add not found")
	}
	for _, want := range []string{`"a"`, `"b"`} {
		if !strings.Contains(string(propsJSON), want) {
			t.Fatalf("expected input schema properties to contain %s, got %s", want, propsJSON)
		}
	}
}

// TestLoadMCPConfig covers parsing both server shapes .mcp.json supports:
// stdio (command/args/env) and HTTP (url/headers).
func TestLoadMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	content := `{
		"mcpServers": {
			"stdio-server": {"command": "myserver", "args": ["--flag"], "env": {"FOO": "bar"}},
			"http-server": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer xyz"}}
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadMCPConfig(path)
	if err != nil {
		t.Fatalf("LoadMCPConfig: %v", err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("expected 2 servers, got %d: %+v", len(cfg.MCPServers), cfg.MCPServers)
	}

	stdio, ok := cfg.MCPServers["stdio-server"]
	if !ok {
		t.Fatal("expected stdio-server in config")
	}
	if stdio.Command != "myserver" || len(stdio.Args) != 1 || stdio.Args[0] != "--flag" || stdio.Env["FOO"] != "bar" {
		t.Fatalf("unexpected stdio server config: %+v", stdio)
	}

	httpSrv, ok := cfg.MCPServers["http-server"]
	if !ok {
		t.Fatal("expected http-server in config")
	}
	if httpSrv.URL != "https://example.com/mcp" || httpSrv.Headers["Authorization"] != "Bearer xyz" {
		t.Fatalf("unexpected http server config: %+v", httpSrv)
	}
}

// TestLoadMCPConfigMissingFile covers the returned-error path for a config
// file that doesn't exist.
func TestLoadMCPConfigMissingFile(t *testing.T) {
	_, err := LoadMCPConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

// TestConnectMCPSkipsServerThatFailsToConnect covers the stated resilience
// requirement: a server that fails to connect (here, a nonexistent command)
// is logged and skipped rather than making ConnectMCP fail outright.
func TestConnectMCPSkipsServerThatFailsToConnect(t *testing.T) {
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"bad": {Command: "definitely-not-a-real-command-xyz-fundi-test"},
	}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{})
	if err != nil {
		t.Fatalf("ConnectMCP: %v", err)
	}
	defer shutdown()

	if defs := r.Definitions(); len(defs) != 0 {
		t.Fatalf("expected no tools registered from a failing server, got %v", defs)
	}
}

// TestConnectMCPSkipsServerWithNoCommandOrURL covers a malformed config
// entry (neither command nor url set) being skipped the same way a
// connection failure is, rather than panicking or propagating an error that
// would take down every other configured server.
func TestConnectMCPSkipsServerWithNoCommandOrURL(t *testing.T) {
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{"empty": {}}}

	r := NewRegistry()
	shutdown, err := ConnectMCP(context.Background(), r, cfg, OutputPolicy{})
	if err != nil {
		t.Fatalf("ConnectMCP: %v", err)
	}
	defer shutdown()

	if defs := r.Definitions(); len(defs) != 0 {
		t.Fatalf("expected no tools registered, got %v", defs)
	}
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
	const oddName = "github.create issue"
	session := connectInMemory(t, newCustomMCPServer("test-server", oddName))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

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
	if !anthropicToolNameRETest.MatchString(want) {
		t.Fatalf("registered name %q does not match Anthropic's tool name grammar", want)
	}

	out, err := r.Execute(context.Background(), want, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error calling %q: %v", want, err)
	}
	if out != "ok:"+oddName {
		t.Fatalf("expected %q, got %q", "ok:"+oddName, out)
	}
}

// TestRegisterMCPServerToolsSkipsOverlongName covers the requirement that a
// built mcp__server__tool name failing Anthropic's grammar even AFTER
// normalization (here: length > 128) is skipped with a warning rather than
// registered invalid - and that the rest of the same server's tools are
// unaffected.
func TestRegisterMCPServerToolsSkipsOverlongName(t *testing.T) {
	longName := strings.Repeat("a", 130)
	session := connectInMemory(t, newCustomMCPServer("test-server", longName, "short"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	names := map[string]bool{}
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}
	for name := range names {
		if len(name) > 128 {
			t.Fatalf("expected no registered name over 128 characters, got %q (%d chars)", name, len(name))
		}
	}
	if !names["mcp__srv__short"] {
		t.Fatalf("expected the other tool on the same server to still be registered, got %v", names)
	}
	if len(names) != 1 {
		t.Fatalf("expected exactly one registered tool (the overlong one skipped), got %v", names)
	}
}

// TestRegisterMCPServerToolsSkipsCollidingNormalizedNames covers the
// collision-handling requirement: two distinct tool names that normalize to
// the same mcp__server__tool string must not both register - the later one
// is skipped (with a warning) rather than silently shadowing the first via
// Registry.Register's overwrite semantics. Only one registration must
// result, and neither call may panic.
func TestRegisterMCPServerToolsSkipsCollidingNormalizedNames(t *testing.T) {
	session := connectInMemory(t, newCustomMCPServer("test-server", "list-items", "list_items"))

	r := NewRegistry()
	if err := registerMCPServerTools(context.Background(), r, "srv", session, OutputPolicy{}, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	count := 0
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "mcp__srv__list_items" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one registration of the colliding name, got %d (defs: %v)", count, r.Definitions())
	}
}

// TestRegisterMCPServerToolsClipsOversizedOutput covers the requirement
// that MCP tool results go through OutputPolicy.Clip like every other
// ToolFunc in this package: an over-budget result is clipped for the model,
// with the FULL result spilled to SpillDir (mirrors bash_test.go's
// TestBashOutputGoesThroughSpillPolicy).
func TestRegisterMCPServerToolsClipsOversizedOutput(t *testing.T) {
	spillDir := t.TempDir()
	full := strings.Repeat("x", 2000)
	session := connectInMemory(t, newBigOutputMCPServer("test-server", "big", full))

	r := NewRegistry()
	p := OutputPolicy{Budget: 200, SpillDir: spillDir}
	if err := registerMCPServerTools(context.Background(), r, "srv", session, p, make(map[string]string)); err != nil {
		t.Fatalf("registerMCPServerTools: %v", err)
	}

	out, err := r.Execute(context.Background(), "mcp__srv__big", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) > 400 {
		t.Fatalf("expected clipped output, got %d bytes", len(out))
	}
	if !strings.Contains(out, "elided") {
		t.Fatalf("expected elision marker, got %q", out)
	}

	entries, err := os.ReadDir(spillDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one spill file, got %d", len(entries))
	}
	spilled, err := os.ReadFile(filepath.Join(spillDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(spilled) != full {
		t.Fatalf("spilled file does not hold the full output: got %d bytes, want %d", len(spilled), len(full))
	}
}
