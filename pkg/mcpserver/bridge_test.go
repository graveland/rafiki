package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
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
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return cs
}

// resultText concatenates the text blocks of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("unexpected non-text content %#v", c)
		}
		b.WriteString(tc.Text)
	}
	return b.String()
}

// canonicalJSON re-marshals v through map[string]any so schemas compared
// across the wire are key-order independent: the SDK decodes InputSchema into
// a map on the client, losing the server's key order.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("canonical marshal: %v", err)
	}
	return string(out)
}

func TestBridgeExposesEveryToolWithItsSchema(t *testing.T) {
	toolA := &fakeTool{name: "task_add", desc: "add a task", schema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`}
	toolB := &fakeTool{name: "task_list", desc: "list tasks", schema: `{"type":"object","properties":{"handle":{"type":"string"}}}`}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{toolA, toolB}, Version: "test"})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
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
		if got.Description != pair.tool.desc {
			t.Errorf("tool %q description = %q, want %q", pair.tool.name, got.Description, pair.tool.desc)
		}
		if canon := canonicalJSON(t, got.InputSchema); canon != canonicalJSON(t, json.RawMessage(pair.schema)) {
			t.Errorf("tool %q schema = %s, want %s", pair.tool.name, canon, pair.schema)
		}
	}
}

func TestBridgeSkipsNilTools(t *testing.T) {
	a := &fakeTool{name: "a", desc: "a", schema: objSchema}
	b := &fakeTool{name: "b", desc: "b", schema: objSchema}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{nil, a, nil, b}, Version: "test"})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("listed tools %v, want [a b]", names)
	}
}

func TestBridgeDescriptionOverrideWins(t *testing.T) {
	overridden := &fakeTool{name: "task_add", desc: "blueprint description", schema: objSchema}
	plain := &fakeTool{name: "task_list", desc: "own description", schema: objSchema}
	cs := bridgeSession(t, Options{
		Tools:        []tools.Tool{overridden, plain},
		Descriptions: map[string]string{"task_add": "this is not your native Task tool"},
		Version:      "test",
	})

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = tool.Description
	}
	if d := got["task_add"]; d != "this is not your native Task tool" {
		t.Errorf("overridden description = %q, want the Descriptions entry", d)
	}
	if d := got["task_list"]; d != "own description" {
		t.Errorf("plain description = %q, want the tool's own", d)
	}
}

func TestBridgeToolFailureIsAnIsErrorResult(t *testing.T) {
	boom := &fakeTool{name: "boom", desc: "fails", schema: objSchema, exec: func(context.Context, tools.ToolInput) (tools.ToolResult, error) {
		return tools.ToolResult{}, errors.New("boom")
	}}
	cs := bridgeSession(t, Options{Tools: []tools.Tool{boom}, Version: "test"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "boom"})
	if err != nil {
		t.Fatalf("tool failure surfaced as a transport error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError = false, want true")
	}
	if text := resultText(t, res); !strings.Contains(text, "boom") {
		t.Errorf("result text %q does not carry the diagnostic", text)
	}
}

func TestBridgeInjectsConversationID(t *testing.T) {
	probe := &fakeTool{name: "probe", desc: "reads the id", schema: objSchema, exec: func(ctx context.Context, _ tools.ToolInput) (tools.ToolResult, error) {
		return tools.NewTextResult(tools.ConversationIDFromContext(ctx)), nil
	}}
	cs := bridgeSession(t, Options{
		Tools:                 []tools.Tool{probe},
		ResolveConversationID: func(context.Context) (string, error) { return "conv-42", nil },
		Version:               "test",
	})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if text := resultText(t, res); text != "conv-42" {
		t.Errorf("tool saw conversation id %q, want conv-42", text)
	}
}

func TestBridgeConversationIDResolveFailureIsAToolError(t *testing.T) {
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
	if err != nil {
		t.Fatalf("resolver failure surfaced as a transport error: %v", err)
	}
	if executed {
		t.Error("tool ran despite the resolver failing")
	}
	if !res.IsError {
		t.Error("IsError = false, want true")
	}
	if text := resultText(t, res); !strings.Contains(text, "no conversation") {
		t.Errorf("result text %q does not carry the diagnostic", text)
	}
}

func TestBridgeAbsentArgumentsBecomeEmptyObject(t *testing.T) {
	// The SDK's own client never sends an absent arguments field
	// (CallTool substitutes {} for nil), so the absent case is driven at the
	// handler seam, with the passthrough case beside it.
	var got string
	probe := &fakeTool{name: "probe", desc: "records input", schema: objSchema, exec: func(_ context.Context, input tools.ToolInput) (tools.ToolResult, error) {
		got = string(input)
		return tools.NewTextResult("ok"), nil
	}}
	handler := handlerFor(probe, nil)
	if _, err := handler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got != "{}" {
		t.Fatalf("tool received %q, want {}", got)
	}

	var passthrough string
	probe.exec = func(_ context.Context, input tools.ToolInput) (tools.ToolResult, error) {
		passthrough = string(input)
		return tools.NewTextResult("ok"), nil
	}
	if _, err := handler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe", Arguments: json.RawMessage(`{"k":"v"}`)}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if passthrough != `{"k":"v"}` {
		t.Fatalf("tool received %q, want the caller's arguments verbatim", passthrough)
	}
}
