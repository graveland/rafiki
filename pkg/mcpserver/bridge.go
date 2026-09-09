package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"

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
	// scope by. It is called on EVERY tool call, including tools that never
	// read the id — a generic bridge cannot see which tools need it — so a
	// resolver with an expensive lookup must memoize. The daemon's resolver
	// (the mcpLedger) already caches per user. An error is returned to the
	// caller as a tool error, not as a transport error.
	ResolveConversationID func(context.Context) (string, error)

	// RegisterSession, when non-nil, is invoked with the calling session on
	// every tool call. The go-sdk's SEP-2575 discover handshake (v1.7.0+)
	// never sends notifications/initialized, so an InitializedHandler alone
	// sees only legacy-handshake clients; the first tool call is the earliest
	// hook both handshakes share, and a session that never calls a tool can
	// never produce the events a per-session hook would serve. The callback
	// must be idempotent per session: it fires on every call, not just the
	// first. The argument is session identity only — no user id, username or
	// request may cross this boundary; whatever the session means to the
	// caller is closed over in the callback, the same rule ServerOptions
	// documents.
	RegisterSession func(*mcp.ServerSession)

	// ServerOptions, when non-nil, is passed straight through to
	// mcp.NewServer, so the caller can attach server-side SDK hooks (e.g. an
	// InitializedHandler) without this bridge growing a field per hook. The
	// bridge never looks inside it: nothing here may carry a user id, a
	// username or an *http.Request — whatever identity a hook needs is closed
	// over by the caller that built the options, keeping this package
	// identity-free.
	ServerOptions *mcp.ServerOptions

	Version string
}

// New builds an MCP server exposing opts.Tools.
func New(opts Options) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki", Title: "rafiki agent control", Version: opts.Version}, opts.ServerOptions)
	for _, t := range opts.Tools {
		// A Materialize that declined returns (nil, nil); a nil-interface
		// deref here is a panic in the daemon's request path.
		if t == nil {
			continue
		}
		tool := t
		schema, ok := acceptableSchema(tool)
		if !ok {
			// The SDK's AddTool panics on a non-object schema; New runs in the
			// daemon's per-request path, so skip the tool instead.
			slog.Warn("mcpserver: skipping tool whose input schema is not a JSON object with type object", "tool", tool.Name())
			continue
		}
		desc := tool.Description()
		if o, ok := opts.Descriptions[tool.Name()]; ok {
			desc = o
		}
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name(),
			Description: desc,
			InputSchema: schema,
		}, handlerFor(tool, opts.ResolveConversationID, opts.RegisterSession))
	}
	return srv
}

// acceptableSchema marshals a tool's input schema and reports whether the
// SDK's AddTool can accept it: nil/empty bytes, undecodable JSON, or a schema
// whose "type" is not exactly "object" would panic there. The guard sits
// ahead of the AddTool call, whose shape is mandated by the bridge contract.
func acceptableSchema(t tools.Tool) (json.RawMessage, bool) {
	raw := json.RawMessage(t.InputSchema().JSON())
	if len(raw) == 0 {
		return nil, false
	}
	var shape struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil || shape.Type != "object" {
		return nil, false
	}
	return raw, true
}

// handlerFor wraps one rafiki tool as an MCP tool handler. A tool failure is
// an isError result carrying the diagnostic, with a nil Go error — never a
// JSON-RPC transport error, and never a successful result carrying the
// diagnostic as text.
func handlerFor(t tools.Tool, resolve func(context.Context) (string, error), register func(*mcp.ServerSession)) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if register != nil && req != nil && req.Session != nil {
			register(req.Session)
		}
		if resolve != nil {
			id, err := resolve(ctx)
			if err != nil {
				return errorResult(err), nil
			}
			// An empty id is indistinguishable from no injection downstream
			// (ConversationIDFromContext returns "" both ways) — never inject
			// it.
			if id != "" {
				ctx = context.WithValue(ctx, tools.ConversationIDKey{}, id)
			}
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
