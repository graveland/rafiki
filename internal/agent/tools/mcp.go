package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"git.graveland.dev/brent/rafiki/agentloop"
)

// mcpClientName is the Implementation.Name fundi advertises to every MCP
// server it connects to.
const mcpClientName = "fundi"

// MCPServerConfig describes one entry in .mcp.json's "mcpServers" map. A
// stdio server sets Command (and optionally Args/Env); an HTTP server sets
// URL (and optionally Headers). Exactly one of Command/URL is expected to be
// set per the .mcp.json convention.
type MCPServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig mirrors the standard .mcp.json shape:
//
//	{"mcpServers": {"name": {"command": "...", "args": [...], "env": {...}}}}
//
// or, for an HTTP server, {"url": "...", "headers": {...}}.
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// LoadMCPConfig reads and parses the .mcp.json file at path.
func LoadMCPConfig(path string) (MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MCPConfig{}, fmt.Errorf("mcp: reading config %s: %w", path, err)
	}
	var cfg MCPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return MCPConfig{}, fmt.Errorf("mcp: parsing config %s: %w", path, err)
	}
	return cfg, nil
}

// ConnectMCP dials every server in cfg (stdio via Command, HTTP via URL),
// lists each server's tools, and registers each one on r as
// mcp__<server>__<tool>, with every character outside [a-zA-Z0-9_] in both
// the server and tool name normalized to underscore (see normalizeMCPName).
// Tool results are passed through p.Clip before being handed to the model.
//
// A server that fails to connect, or whose tool list can't be fetched, is
// logged and skipped: MCP servers are third-party subprocesses/endpoints and
// a single broken one must not prevent the rest from being usable or take
// down the agent. ConnectMCP itself therefore does not fail for per-server
// problems; its error return is reserved for conditions that would leave the
// registry in an inconsistent state (there are currently none, but the
// signature keeps that door open rather than encoding "always nil" into
// every caller).
//
// Two more per-tool failure modes are handled the same way, with a
// slog.Warn rather than slog.Error since these are expected in the wild
// rather than exceptional: a normalized name that still fails the
// Anthropic API's name grammar (e.g. exceeds 128 characters) is skipped
// rather than registered invalid, and a normalized name that collides with
// one already registered earlier in this same ConnectMCP call is skipped
// rather than silently shadowing the first (Registry.Register overwrites
// same-name entries with no warning of its own).
//
// The returned shutdown func closes every session that was successfully
// connected. It is always non-nil and safe to call even if every server was
// skipped.
func ConnectMCP(ctx context.Context, r *Registry, cfg MCPConfig, p OutputPolicy) (func(), error) {
	var sessions []*mcp.ClientSession

	// registeredNames tracks every mcp__server__tool name registered so far
	// across ALL servers processed by this ConnectMCP call, mapping it to a
	// description of the tool that claimed it first. Two distinct
	// (server, tool) pairs can normalize to the same name (e.g. servers
	// "my-server"/"my_server", or tools "list-items"/"list_items" on the
	// same server); since Registry.Register silently replaces a duplicate
	// name, the later one must be skipped here instead, with a warning
	// naming both sources.
	registeredNames := make(map[string]string)

	for name, sc := range cfg.MCPServers {
		session, err := dialMCPServer(ctx, name, sc)
		if err != nil {
			slog.Error("agent/tools: mcp: failed to connect to server, skipping", "server", name, "error", err)
			continue
		}

		if err := registerMCPServerTools(ctx, r, name, session, p, registeredNames); err != nil {
			slog.Error("agent/tools: mcp: failed to list tools, skipping server", "server", name, "error", err)
			if cerr := session.Close(); cerr != nil {
				slog.Warn("agent/tools: mcp: error closing session after tool-list failure", "server", name, "error", cerr)
			}
			continue
		}

		sessions = append(sessions, session)
	}

	shutdown := func() {
		for _, session := range sessions {
			if err := session.Close(); err != nil {
				slog.Warn("agent/tools: mcp: error closing session", "error", err)
			}
		}
	}
	return shutdown, nil
}

// dialMCPServer connects to a single configured server, choosing a stdio or
// HTTP transport based on which of Command/URL is set.
func dialMCPServer(ctx context.Context, name string, sc MCPServerConfig) (*mcp.ClientSession, error) {
	var transport mcp.Transport
	switch {
	case sc.Command != "":
		cmd := exec.Command(sc.Command, sc.Args...)
		if len(sc.Env) > 0 {
			env := os.Environ()
			for k, v := range sc.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case sc.URL != "":
		httpClient := http.DefaultClient
		if len(sc.Headers) > 0 {
			httpClient = &http.Client{Transport: headerRoundTripper{headers: sc.Headers}}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: httpClient}
	default:
		return nil, fmt.Errorf("mcp: server %q has neither command nor url configured", name)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: mcpClientName}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connecting to server %q: %w", name, err)
	}
	return session, nil
}

// headerRoundTripper injects a fixed set of headers (e.g. Authorization) on
// every outgoing request, since StreamableClientTransport has no built-in
// headers field.
type headerRoundTripper struct {
	headers map[string]string
}

func (t headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// registerMCPServerTools lists session's tools and registers each on r under
// its normalized mcp__<server>__<tool> name. All tools registered by one
// call share a spillCounter (see newMCPToolFunc), giving each a distinct
// fallback spill name when called outside a real agentloop turn.
//
// registeredNames tracks every name registered so far across the whole
// ConnectMCP call (not just this server) so a normalization collision — two
// distinct tools whose names fold to the same mcp__server__tool string — is
// caught and the later one skipped, rather than silently shadowing the
// first via Registry.Register's overwrite semantics.
func registerMCPServerTools(ctx context.Context, r *Registry, serverName string, session *mcp.ClientSession, p OutputPolicy, registeredNames map[string]string) error {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcp: listing tools for server %q: %w", serverName, err)
	}

	normServer := normalizeMCPName(serverName)
	var spillCounter atomic.Int64
	for _, t := range res.Tools {
		def, err := mcpToolDef(normServer, t)
		if err != nil {
			slog.Error("agent/tools: mcp: skipping tool with unusable input schema", "server", serverName, "tool", t.Name, "error", err)
			continue
		}

		name := def.OfTool.Name
		if !anthropicToolNameRE.MatchString(name) {
			// A single invalid name 400s the ENTIRE tools array on the next
			// turn, disabling every tool - so this tool is skipped rather
			// than registered under a name the API will reject.
			slog.Warn("agent/tools: mcp: skipping tool whose normalized name is invalid for the Anthropic API", "server", serverName, "tool", t.Name, "normalized_name", name)
			continue
		}
		if origin, dup := registeredNames[name]; dup {
			slog.Warn("agent/tools: mcp: skipping tool whose normalized name collides with an already-registered tool", "server", serverName, "tool", t.Name, "normalized_name", name, "colliding_with", origin)
			continue
		}

		registeredNames[name] = fmt.Sprintf("server %q, tool %q", serverName, t.Name)
		r.Register(def, newMCPToolFunc(session, t.Name, name, p, &spillCounter))
	}
	return nil
}

// mcpToolDef builds the Anthropic tool definition for one MCP tool, passing
// its input schema through verbatim (see ConnectMCP's doc comment).
func mcpToolDef(normServer string, t *mcp.Tool) (anthropic.ToolUnionParam, error) {
	name := fmt.Sprintf("mcp__%s__%s", normServer, normalizeMCPName(t.Name))

	var schema anthropic.ToolInputSchemaParam
	if t.InputSchema != nil {
		schemaBytes, err := json.Marshal(t.InputSchema)
		if err != nil {
			return anthropic.ToolUnionParam{}, fmt.Errorf("marshaling input schema for %q: %w", t.Name, err)
		}
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			return anthropic.ToolUnionParam{}, fmt.Errorf("converting input schema for %q: %w", t.Name, err)
		}
	}

	def := anthropic.ToolUnionParamOfTool(schema, name)
	def.OfTool.Description = anthropic.String(t.Description)
	return def, nil
}

// nonTokenChars matches every rune not allowed to appear as-is in a
// normalized MCP name segment. Anthropic tool names must match
// ^[a-zA-Z0-9_-]{1,128}$ (hyphens are technically legal there), but
// third-party MCP tool/server names commonly use other separators too (a
// literal dot is common, e.g. "github.create_issue") which would otherwise
// build an INVALID mcp__ name and 400 the entire tools array. So every
// character outside [a-zA-Z0-9_] - not just hyphen - is folded to
// underscore, which also keeps names consistent with this project's
// dispatch logic (it pattern-matches tool names in their underscore form).
var nonTokenChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// anthropicToolNameRE is the exact grammar the Anthropic API enforces for
// tool names. It is checked against the final built mcp__server__tool name
// (not just its segments) because normalization alone doesn't guarantee
// validity - e.g. length: a server or tool name long enough to push the
// combined name past 128 characters is still invalid even though every
// character in it is otherwise legal.
var anthropicToolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// normalizeMCPName folds every character outside [a-zA-Z0-9_] to an
// underscore (see nonTokenChars).
func normalizeMCPName(s string) string {
	return nonTokenChars.ReplaceAllString(s, "_")
}

// newMCPToolFunc returns the ToolFunc that dispatches a call to toolName on
// session. registeredName (the mcp__server__tool form) is used as the
// fallback spill file name when agentloop.ToolCallID(ctx) is empty (e.g. a
// direct Execute call outside a real agentloop turn) - spillCounter keeps
// concurrent fallback names distinct without a mutex, mirroring bash.go.
func newMCPToolFunc(session *mcp.ClientSession, toolName, registeredName string, p OutputPolicy, spillCounter *atomic.Int64) ToolFunc {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var args any
		if len(input) > 0 {
			args = json.RawMessage(input)
		}

		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: args})
		if err != nil {
			return "", fmt.Errorf("mcp: calling tool %q: %w", toolName, err)
		}

		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}

		spillName := agentloop.ToolCallID(ctx)
		if spillName == "" {
			spillName = fmt.Sprintf("%s_%d", registeredName, spillCounter.Add(1))
		}
		out := p.Clip(text.String(), spillName)

		if res.IsError {
			// Per the MCP spec (and this SDK's CallToolResult.IsError doc),
			// a tool-level error is reported inside Content, not as a
			// protocol error - CallTool above returned err == nil. Turning
			// it into a Go error here is what lets rafiki's agentloop mark
			// this an is_error tool result the model can see and recover
			// from, matching every other ToolFunc in this package.
			return "", fmt.Errorf("mcp: tool %q returned an error: %s", toolName, out)
		}
		return out, nil
	}
}
