package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

type fakeTool struct {
	name   string
	desc   string
	schema string
	exec   func(context.Context, tools.ToolInput) (tools.ToolResult, error)
}

func (f *fakeTool) Name() string              { return f.name }
func (f *fakeTool) Description() string       { return f.desc }
func (f *fakeTool) InputSchema() tools.Schema { return tools.SchemaFromRaw(json.RawMessage(f.schema)) }

func (f *fakeTool) Execute(ctx context.Context, input tools.ToolInput) (tools.ToolResult, error) {
	if f.exec != nil {
		return f.exec(ctx, input)
	}
	return tools.NewTextResult("ok"), nil
}

const objSchema = `{"type":"object","properties":{"k":{"type":"string"}}}`

// bridgeSession runs an in-memory MCP round trip against New's server and
// returns the client session. The server must be connected before the client,
// because the client initializes the MCP session during Connect.
func bridgeSession(t *testing.T, opts Options) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	srv := New(opts)
	ctx := t.Context()
	done := make(chan error, 1)
	t.Cleanup(func() { <-done })
	go func() { done <- srv.Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	assert.NewAborting(t).NoError(err, "client connect")
	return cs
}

// resultText concatenates the text blocks of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		assert.NewAborting(t).True(ok, "unexpected non-text content %#v", c)
		b.WriteString(tc.Text)
	}
	return b.String()
}

// canonicalJSON re-marshals v through map[string]any so schemas compared
// across the wire are key-order independent: the SDK decodes InputSchema into
// a map on the client, losing the server's key order.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	c := assert.NewAborting(t)
	b, err := json.Marshal(v)
	c.NoError(err, "marshal %v", v)
	var decoded any
	c.NoError(json.Unmarshal(b, &decoded), "unmarshal %s", b)
	out, err := json.Marshal(decoded)
	c.NoError(err, "canonical marshal")
	return string(out)
}

func TestBridgeExposesEveryToolWithItsSchema(t *testing.T) {
	c := assert.NewCollecting(t)
	toolA := &fakeTool{name: "task_add", desc: "add a task", schema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`}
	toolB := &fakeTool{name: "task_list", desc: "list tasks", schema: `{"type":"object","properties":{"handle":{"type":"string"}}}`}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{toolA, toolB}, Version: "test"})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	c.Require().NoError(err, "ListTools")
	seen := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		if _, dup := seen[tool.Name]; dup {
			t.Fatalf("tool %q listed more than once", tool.Name)
		}
		seen[tool.Name] = tool
	}
	for _, pair := range []struct {
		tool   *fakeTool
		schema string
	}{{toolA, toolA.schema}, {toolB, toolB.schema}} {
		got, ok := seen[pair.tool.name]
		if !ok {
			t.Fatalf("tool %q not listed", pair.tool.name)
		}
		c.Eq(pair.tool.desc, got.Description, "tool %q description = %q, want", pair.tool.name, got.Description)
		canon := canonicalJSON(t, got.InputSchema)
		c.Eq(canonicalJSON(t, json.RawMessage(pair.schema)), canon, "tool %q schema = %s, want %s", pair.tool.name, canon, pair.schema)
	}
}

func TestBridgeSkipsNilTools(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeTool{name: "a", desc: "a", schema: objSchema}
	b := &fakeTool{name: "b", desc: "b", schema: objSchema}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{nil, a, nil, b}, Version: "test"})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	c.NoError(err, "ListTools")
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	c.False(len(names) != 2 || names[0] != "a" || names[1] != "b", "listed tools %v, want [a b]", names)
}

func TestBridgeSkipsAToolWhoseSchemaIsNotAnObject(t *testing.T) {
	c := assert.NewAborting(t)
	// A schema the SDK's AddTool would panic on (no "type":"object", or an
	// empty object after Schema's builder path) must drop the tool with a
	// warn, never panic New — the daemon builds one server per request.
	noType := &fakeTool{name: "no_type", desc: "missing type", schema: `{"properties":{"k":{"type":"string"}}}`}
	empty := &fakeTool{name: "empty_type", desc: "zero-value schema", schema: ""}
	good := &fakeTool{name: "good", desc: "an object", schema: objSchema}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{noType, empty, good}, Version: "test"})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	c.NoError(err, "ListTools")
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	c.False(len(names) != 1 || names[0] != "good", "listed tools %v, want [good]", names)

	// The skipped names must be genuinely absent from the server, not merely
	// unlisted: calling one is an MCP error response, not a panic.
	for _, name := range []string{"no_type", "empty_type"} {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name}); err == nil {
			t.Errorf("call to skipped tool %q returned no error", name)
		}
	}
}

func TestBridgeDescriptionOverrideWins(t *testing.T) {
	c := assert.NewCollecting(t)
	overridden := &fakeTool{name: "task_add", desc: "blueprint description", schema: objSchema}
	plain := &fakeTool{name: "task_list", desc: "own description", schema: objSchema}
	cs := bridgeSession(t, Options{
		Tools:        []tools.Tool{overridden, plain},
		Descriptions: map[string]string{"task_add": "this is not your native Task tool"},
		Version:      "test",
	})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	c.Require().NoError(err, "ListTools")
	got := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = tool.Description
	}
	c.Eq("this is not your native Task tool", got["task_add"], "overridden description")
	c.Eq("own description", got["task_list"], "plain description")
}

func TestBridgeToolFailureIsAnIsErrorResult(t *testing.T) {
	c := assert.NewCollecting(t)
	boom := &fakeTool{name: "boom", desc: "fails", schema: objSchema, exec: func(context.Context, tools.ToolInput) (tools.ToolResult, error) {
		return tools.ToolResult{}, errors.New("boom")
	}}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{boom}, Version: "test"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "boom"})
	c.Require().NoError(err, "tool failure surfaced as a transport error")
	c.True(res.IsError, "IsError = false, want true")
	c.StrContains(resultText(t, res), "boom", "result text")
}

func TestBridgeInjectsConversationID(t *testing.T) {
	c := assert.NewCollecting(t)
	probe := &fakeTool{name: "probe", desc: "reads the id", schema: objSchema, exec: func(ctx context.Context, _ tools.ToolInput) (tools.ToolResult, error) {
		return tools.NewTextResult(tools.ConversationIDFromContext(ctx)), nil
	}}
	cs := bridgeSession(t, Options{
		Tools:                 []tools.Tool{probe},
		ResolveConversationID: func(context.Context) (string, error) { return "conv-42", nil },
		Version:               "test",
	})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"})
	c.Require().NoError(err, "CallTool")
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	c.Eq("conv-42", resultText(t, res), "tool saw conversation id")
}

func TestBridgeConversationIDResolveFailureIsAToolError(t *testing.T) {
	c := assert.NewCollecting(t)
	executed := false
	probe := &fakeTool{name: "probe", desc: "must not run", schema: objSchema, exec: func(context.Context, tools.ToolInput) (tools.ToolResult, error) {
		executed = true
		return tools.NewTextResult("ran"), nil
	}}
	cs := bridgeSession(t, Options{
		Tools:                 []tools.Tool{probe},
		ResolveConversationID: func(context.Context) (string, error) { return "", errors.New("no conversation") },
		Version:               "test",
	})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"})
	c.Require().NoError(err, "resolver failure surfaced as a transport error")
	c.False(executed, "tool ran despite the resolver failing")
	c.True(res.IsError, "IsError = false, want true")
	c.StrContains(resultText(t, res), "no conversation", "result text")
}

func TestBridgeAbsentArgumentsBecomeEmptyObject(t *testing.T) {
	c := assert.NewAborting(t)
	// The SDK's own client never sends an absent arguments field
	// (CallTool substitutes {} for nil), so the absent case is driven at the
	// handler seam, with the passthrough case beside it.
	var got string
	probe := &fakeTool{name: "probe", desc: "records input", schema: objSchema, exec: func(_ context.Context, input tools.ToolInput) (tools.ToolResult, error) {
		got = string(input)
		return tools.NewTextResult("ok"), nil
	}}
	handler := handlerFor(probe, nil, nil)
	if _, err := handler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	c.Eq("{}", got, "tool received")

	var passthrough string
	probe.exec = func(_ context.Context, input tools.ToolInput) (tools.ToolResult, error) {
		passthrough = string(input)
		return tools.NewTextResult("ok"), nil
	}
	_, err := handler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe", Arguments: json.RawMessage(`{"k":"v"}`)}})
	c.NoError(err, "handler")
	c.Eq(`{"k":"v"}`, passthrough, "tool received")
}

// TestBridgeRegisterSessionFiresWithTheCallingSession pins the SEP-2575
// registration seam: the go-sdk's discover handshake (v1.7.0+) never sends
// notifications/initialized, so the caller's only server-side sight of a
// modern session is the tool request it sends. The hook must receive that
// session on every call — idempotency is the caller's problem, not the
// bridge's — and a nil Options.RegisterSession must leave the handler
// working.
func TestBridgeRegisterSessionFiresWithTheCallingSession(t *testing.T) {
	c := assert.NewAborting(t)
	probe := &fakeTool{name: "probe", desc: "noop", schema: objSchema}

	var registered []*mcp.ServerSession
	opts := Options{
		Tools: []tools.Tool{probe},
		RegisterSession: func(ss *mcp.ServerSession) {
			registered = append(registered, ss)
		},
	}
	cs := bridgeSession(t, opts)
	for range 2 {
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe"})
		c.NoError(err, "call")
	}
	// Both calls must have seen the SDK's own session — the same one twice,
	// since one client session drives both calls.
	c.Len(registered, 2, "RegisterSession fired %d times over two calls, want 2", len(registered))
	c.False(registered[0] == nil || registered[0] != registered[1], "RegisterSession must receive the calling session, consistently")

	// Nil RegisterSession must not perturb the tool path.
	plain := bridgeSession(t, Options{Tools: []tools.Tool{probe}})
	_, err := plain.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe"})
	c.NoError(err, "call with no hook")
}
