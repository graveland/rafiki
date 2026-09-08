package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// Options configures one MCP server instance. Every field is resolved for ONE
// caller before the server is built: the whole package exists to keep a
// caller's identity in a closure rather than in a method parameter.
type Options struct {
	// Tools are the already-materialized rafiki tools this server exposes.
	// Materialization happens in the daemon, because that is where the bound
	// AgentSpawner lives.
	Tools []tools.Tool

	// Descriptions overrides a tool's model-facing description by name. The
	// MCP surface needs disambiguation text ("this is not your native Task
	// tool") that would be nonsense inside a fundi child, so the override
	// lives here rather than in the blueprint.
	Descriptions map[string]string

	// ResolveConversationID supplies the conversation id the task_* tools
	// scope by, resolved lazily on the first call that needs it. nil means no
	// id is injected. An error is returned to the caller as a tool error, not
	// as a transport error.
	ResolveConversationID func(context.Context) (string, error)

	Version string
}

// New builds an MCP server exposing opts.Tools.
func New(opts Options) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki", Title: "rafiki agent control", Version: opts.Version}, nil)
	for _, t := range opts.Tools {
		// A Materialize that declined returns (nil, nil); a nil-interface
		// deref here is a panic in the daemon's request path.
		if t == nil {
			continue
		}
		tool := t
		desc := tool.Description()
		if o, ok := opts.Descriptions[tool.Name()]; ok {
			desc = o
		}
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name(),
			Description: desc,
			InputSchema: json.RawMessage(tool.InputSchema().JSON()),
		}, handlerFor(tool, opts.ResolveConversationID))
	}
	return srv
}

// handlerFor wraps one rafiki tool as an MCP tool handler. A tool failure is
// an isError result carrying the diagnostic, with a nil Go error — never a
// JSON-RPC transport error, and never a successful result carrying the
// diagnostic as text.
func handlerFor(t tools.Tool, resolve func(context.Context) (string, error)) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if resolve != nil {
			id, err := resolve(ctx)
			if err != nil {
				return errorResult(err), nil
			}
			ctx = context.WithValue(ctx, tools.ConversationIDKey{}, id)
		}
		// Several blueprints take no arguments and a client may omit
		// `arguments` entirely, while json.Unmarshal on an empty RawMessage
		// errors.
		args := json.RawMessage("{}")
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			args = req.Params.Arguments
		}
		res, err := t.Execute(ctx, tools.ToolInput(args))
		if err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text}}}, nil
	}
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
