package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/toolmeta"
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
	// ViaExecutor, when set, means this server lives on an EXECUTOR's machine
	// rather than this process's: either a process the executor hosts by a
	// name its operator declared (Command), or an HTTP endpoint its operator
	// declared (Proxy). The entry's own command/args/env/url must then be
	// absent — the argv and the environment belong to the machine that holds
	// the credentials, and nothing secret travels to the daemon.
	ViaExecutor *MCPViaExecutor `json:"via_executor,omitempty"`
}

// MCPViaExecutor says where a server reachable only through the executor plane
// lives, and which declared name on that machine serves it.
type MCPViaExecutor struct {
	// Selector picks the executor(s) by label, as every other selector does.
	// A malformed selector EXCLUDES rather than admits.
	Selector string `json:"selector"`
	// Exactly one of Command (a --mcp-command name on the executor) or Proxy
	// (a --proxy name on the executor).
	Command string `json:"command,omitempty"`
	Proxy   string `json:"proxy,omitempty"`
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
	// A via_executor entry is refused HERE, by name, rather than at dial time:
	// a config error that degrades into "no tools" is invisible, and this one
	// is a deploy mistake, not third-party fragility.
	for name, sc := range cfg.MCPServers {
		if err := sc.validate(name); err != nil {
			return MCPConfig{}, fmt.Errorf("mcp: config %s: %w", path, err)
		}
	}
	return cfg, nil
}

// validate refuses a server whose shape cannot be honoured, naming the server.
//
// via_executor's own command/proxy are SIBLINGS of the entry's top-level
// command/args/env/url, and only one set may be present: the argv and the
// environment live on the executor (they are what the operator declared, and
// what the daemon must not be able to change), so an entry carrying both is
// ambiguous about which machine runs what.
func (sc MCPServerConfig) validate(name string) error {
	ve := sc.ViaExecutor
	if ve == nil {
		return nil
	}
	if sc.Command != "" || len(sc.Args) > 0 || len(sc.Env) > 0 || sc.URL != "" {
		return fmt.Errorf("server %q: via_executor cannot be combined with the entry's own command/args/env/url — the argv and environment live on the executor", name)
	}
	switch {
	case ve.Command == "" && ve.Proxy == "":
		return fmt.Errorf("server %q: via_executor needs exactly one of command (a --mcp-command name on the executor) or proxy (a --proxy name on the executor)", name)
	case ve.Command != "" && ve.Proxy != "":
		return fmt.Errorf("server %q: via_executor sets both command and proxy; exactly one is allowed", name)
	}
	if ve.Selector == "" {
		return fmt.Errorf("server %q: via_executor needs a selector naming the machine the server lives on", name)
	}
	return nil
}

// ViaExecutorNames returns the names of the servers in cfg that reach their
// transport through the executor plane.
func (cfg MCPConfig) ViaExecutorNames() []string {
	var out []string
	for name, sc := range cfg.MCPServers {
		if sc.ViaExecutor != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FilterMCPServers narrows cfg to the comma-separated allowlist. Empty or "*"
// means all (no filtering, cfg returned as-is); any other value keeps only the
// named servers, dropping the rest before anything is dialled.
//
// Exported because the daemon must apply the SAME allowlist when it resolves
// via_executor entries at spawn: a server this child may not use must not be
// able to refuse its spawn.
func FilterMCPServers(cfg map[string]MCPServerConfig, allowlist string) map[string]MCPServerConfig {
	if allowlist == "" || allowlist == "*" {
		return cfg
	}
	allowed := make(map[string]bool, len(cfg))
	for _, name := range strings.Split(allowlist, ",") {
		allowed[name] = true
	}
	out := make(map[string]MCPServerConfig, len(cfg))
	for name, sc := range cfg {
		if allowed[name] {
			out[name] = sc
		}
	}
	return out
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
// Each session is wrapped in mcpServerSession: it is expected to outlive the
// server's own session (servers MAY terminate at any time; the SDK treats
// that as terminal), so a dead session is redialed through the same config
// on the next tool call rather than bricking the server's tools for the
// agent's lifetime.
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
func ConnectMCP(ctx context.Context, r *Registry, cfg MCPConfig, p OutputPolicy, env []string, dial MCPDialer) (func(), error) {
	var refs []*mcpServerSession

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
		session, health, err := dialMCPServer(ctx, name, sc, env, dial)
		if err != nil {
			// The reason is named, not swallowed: for a via_executor entry the
			// operator needs to see WHICH resolution step failed.
			slog.Error("agent/tools: mcp: failed to connect to server, skipping", "server", name, "error", err)
			continue
		}

		ref := &mcpServerSession{name: name, sess: session, health: health, dial: func(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
			return dialMCPServer(ctx, name, sc, env, dial)
		}}

		if err := registerMCPServerTools(ctx, r, name, ref, p, registeredNames); err != nil {
			slog.Error("agent/tools: mcp: failed to list tools, skipping server", "server", name, "error", err)
			ref.close()
			continue
		}

		refs = append(refs, ref)
	}

	shutdown := func() {
		for _, ref := range refs {
			ref.close()
		}
	}
	return shutdown, nil
}

// MCPDialer builds the transport for one via_executor server, resolving the
// machine it lives on. The daemon supplies it because pkg/execpool already
// imports this package, so the dependency cannot run the other way — and the
// resolution happens ONCE, at spawn, with the chosen executor id closed into
// the returned dialer: a child's MCP server cannot hop machines mid-life.
//
// A nil dialer on a via_executor server is an error, never a silent direct
// dial: the daemon's own localhost is not the machine the entry names.
type MCPDialer func(ctx context.Context, name string, sc MCPServerConfig) (mcp.Transport, error)

// MCPServerHealth is the one-method view this package takes of a transport that
// can report an OBSERVED process exit — execpool's MCPStdioTransport is the
// implementation. It is a flag, not an error identity: the SDK wraps and
// unwraps errors on the way up, so an error value would be a rule that
// silently stops holding after an SDK bump.
type MCPServerHealth interface {
	// Exited reports whether the server process was seen to end, and how.
	Exited() (status, tail string, ok bool)
}

// Keepalive: an MCP ping every mcpKeepAlive, closing the session after
// mcpKeepAliveFailures consecutive misses. This catches the one failure the
// executor cannot see — a wedged process that is alive and holding its pipe —
// and closing the session closes the stream, which makes the executor kill it.
const (
	mcpKeepAlive         = 30 * time.Second
	mcpKeepAliveFailures = 2
)

// mcpClientOptions is the ONE set of client options every MCP session uses,
// both transports alike: a wedged server is a wedged server whether it is a
// subprocess or an endpoint. KeepAlive pings every mcpKeepAlive with a 15s
// timeout of its own, tolerates mcpKeepAliveFailures consecutive misses, and
// stops quietly if the peer answers ErrMethodNotFound — an older server, which
// is correct.
func mcpClientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		KeepAlive:                 mcpKeepAlive,
		KeepAliveFailureThreshold: mcpKeepAliveFailures,
		// The SDK's own diagnostics — a keepalive miss, a session closing —
		// go to this daemon's log rather than to a logger nobody reads. Without
		// it, a wedged server's death is invisible until a tool call fails.
		Logger: slog.Default(),
	}
}

// dialMCPServer connects to a single configured server, choosing a via_executor,
// stdio or HTTP transport based on the config. A stdio server is spawned under
// env (ToolOpts.Env) when one is pinned, else the process environment — the same
// base every other subprocess-spawning tool uses.
//
// The returned health is nil for a transport that cannot report an observed
// exit (a local stdio subprocess, an HTTP endpoint); call() reads it to decide
// whether an error is retryable.
func dialMCPServer(ctx context.Context, name string, sc MCPServerConfig, env []string, dial MCPDialer) (*mcp.ClientSession, MCPServerHealth, error) {
	var transport mcp.Transport
	switch {
	case sc.ViaExecutor != nil:
		if dial == nil {
			return nil, nil, fmt.Errorf("mcp: server %q: via_executor requires the executor plane (set RAFIKI_EXECUTORS_ENABLED=1 and RAFIKI_DB)", name)
		}
		tr, err := dial(ctx, name, sc)
		if err != nil {
			return nil, nil, fmt.Errorf("mcp: server %q: %w", name, err)
		}
		transport = tr
	case sc.Command != "":
		cmd := exec.Command(sc.Command, sc.Args...)
		cmd.Env = mcpServerEnv(env, sc.Env)
		transport = &mcp.CommandTransport{Command: cmd}
	case sc.URL != "":
		httpClient := http.DefaultClient
		if len(sc.Headers) > 0 {
			httpClient = &http.Client{Transport: headerRoundTripper{headers: sc.Headers}}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: httpClient}
	default:
		return nil, nil, fmt.Errorf("mcp: server %q has neither command nor url configured", name)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: mcpClientName}, mcpClientOptions())
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: connecting to server %q: %w", name, err)
	}
	var health MCPServerHealth
	if h, ok := transport.(MCPServerHealth); ok {
		health = h
	}
	return session, health, nil
}

// mcpRedialCooldown throttles reconnection attempts after a FAILED dial: a
// dead server would otherwise be re-dialed on every tool call the model
// makes (connection-refused fails in milliseconds, so a hot retry loop is
// cheap to enter). Successful dials do not set it — a session that dies
// moments after connecting is redialed immediately, since each attempt costs
// a real HTTP round trip, not a hammering loop.
const mcpRedialCooldown = 2 * time.Second

// mcpServerSession couples one configured server with its live client
// session and knows how to replace a dead one.
//
// The go-sdk treats a server-side session termination as TERMINAL: per the
// MCP spec a server MAY terminate a session at any time, after which a
// reconnecting client gets HTTP 404; the SDK maps that to ErrSessionMissing,
// fails the connection, and every later CallTool on it returns "client is
// closing" forever (its own docs cite this as too strict — issue #683 — but
// neither v1.7.0 nor v1.8.0 re-initializes). A fundi agent holds its MCP
// sessions for its whole lifetime — days — so any server restart or session
// eviction would otherwise permanently brick every mcp__ tool from that
// server while Definitions() still advertises them.
//
// call() therefore treats a dead-session error as recoverable: drop the
// session, dial a fresh one through the same config, and retry the call
// once. Tool REGISTRATION is deliberately not refreshed on redial: the
// registered names must stay stable for the model across the agent's
// lifetime, and a server that starts answering differently mid-session is a
// problem for the human to notice, not for a retry to paper over.
type mcpServerSession struct {
	name string
	// dial (re)connects to the server. Derived from the MCPServerConfig in
	// production; tests inject an in-memory equivalent.
	dial func(context.Context) (*mcp.ClientSession, MCPServerHealth, error)

	mu   sync.Mutex
	sess *mcp.ClientSession
	// health is the CURRENT session's exit record, if its transport has one.
	// call() reads it to tell an OBSERVED process exit from an ambiguous
	// transport death: the first must not be retried (replaying a
	// non-idempotent tool against a server that just died is a side effect the
	// model has to see), the second may be.
	health     MCPServerHealth
	lastRedial time.Time
}

// current returns the live session, dialing a new one if the stored session
// was dropped. Failed dials set mcpRedialCooldown; a call inside the
// cooldown window fails fast rather than re-attempting the dial.
func (s *mcpServerSession) current(ctx context.Context) (*mcp.ClientSession, MCPServerHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		return s.sess, s.health, nil
	}
	if !s.lastRedial.IsZero() && time.Since(s.lastRedial) < mcpRedialCooldown {
		return nil, nil, fmt.Errorf("mcp: server %q is unreachable (redial attempted %v ago)", s.name, time.Since(s.lastRedial).Round(time.Millisecond))
	}
	sess, health, err := s.dial(ctx)
	if err != nil {
		s.lastRedial = time.Now()
		return nil, nil, fmt.Errorf("mcp: redialing server %q: %w", s.name, err)
	}
	s.sess = sess
	s.health = health
	return sess, health, nil
}

// noteDead drops the stored session if it is still the one that failed —
// a concurrent redial may have already replaced it. The failed session
// object itself is simply dropped: the SDK's connection loops have already
// exited by the time its errors reach us (terminal failure is signaled
// before CallTool returns), and Close on it would be a no-op at best.
func (s *mcpServerSession) noteDead(sess *mcp.ClientSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == sess {
		s.sess = nil
	}
}

// call performs one CallTool against the live session, recovering once from
// a dead session by redialing and retrying. Every other error — including
// tool-level failures (which arrive as IsError results, not Go errors) and
// caller-side context cancellation — passes through untouched.
func (s *mcpServerSession) call(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	sess, health, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	// The exit record is read BEFORE the call and consulted on the SAME
	// transport after it. Both halves matter:
	//
	//   - before, because a server that died while the session sat IDLE has an
	//     exit record too, and that death is not this call's: the next call is
	//     supposed to redial silently (the agent never learns of it). Only an
	//     exit observed DURING the call suppresses the retry.
	//   - the same transport, because a concurrent call may have redialled and
	//     replaced the session; the record that explains THIS failure belongs to
	//     the session that failed.
	_, _, exitedBefore := exitOf(health)
	res, err := sess.CallTool(ctx, params)
	if err == nil || !deadSession(err) {
		return res, err
	}
	if status, tail, ok := exitOf(health); ok && !exitedBefore {
		// The executor SAW the process end while this call was in flight.
		// Retrying would replay a possibly non-idempotent tool against a server
		// that just died, and would repeat itself on every call while it
		// crash-loops. The session is dropped so the next call redials.
		s.noteDead(sess)
		if tail == "" {
			return nil, fmt.Errorf("mcp: calling tool %q: server %q exited (%s); not retried", params.Name, s.name, status)
		}
		return nil, fmt.Errorf("mcp: calling tool %q: server %q exited (%s): %s; not retried", params.Name, s.name, status, tail)
	}
	s.noteDead(sess)
	slog.Warn("agent/tools: mcp: session to server died; redialing", "server", s.name)
	fresh, _, rerr := s.current(ctx)
	if rerr != nil {
		return nil, fmt.Errorf("mcp: calling tool %q: %w (session to server %q was dead and redial failed: %v)", params.Name, err, s.name, rerr)
	}
	return fresh.CallTool(ctx, params)
}

// exitOf reports whether health's server process was seen to end, and how. It
// reads the health FLAG rather than an error value, because the SDK wraps and
// unwraps errors on the way up — an error identity would be a rule that
// silently stops holding after an SDK bump. A nil health (a local subprocess, an
// HTTP endpoint) never reports an exit.
func exitOf(health MCPServerHealth) (status, tail string, ok bool) {
	if health == nil {
		return "", "", false
	}
	return health.Exited()
}

// deadSession reports whether err means "this session is finished and no
// amount of retrying it will help" — the signal to redial. Two public
// sentinels carry most terminal shapes: ErrSessionMissing (HTTP 404 on a
// reconnect — the server terminated the session) and ErrConnectionClosed,
// which mcp.call wraps AROUND the internal jsonrpc2 closing errors
// ("client is closing"/"server is closing"). The io.EOF pair covers the
// transport dying under an in-flight call, which no sentinel is guaranteed
// to wrap on every transport (the in-memory pair surfaces a bare io.EOF);
// a live connection never legitimately ends a call with EOF. ErrClosedPipe
// and net.ErrClosed are the same signal one step earlier, in the race
// against the client's own read loop: after a server-side death the call
// whose write reaches the dead pipe first surfaces io: read/write on
// closed pipe rather than the terminal error the read loop would have
// produced, and a network transport reset by the server surfaces use of
// closed network connection the same way. A live session never
// legitimately ends a call with any of them, which is the whole test this
// function makes. Caller-side context cancellation is excluded: the CALL
// gave up, the session did not die.
func deadSession(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, mcp.ErrSessionMissing) ||
		errors.Is(err, mcp.ErrConnectionClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed)
}

// close drops the live session, if any. Safe to call repeatedly.
func (s *mcpServerSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		if err := s.sess.Close(); err != nil {
			slog.Warn("agent/tools: mcp: error closing session", "server", s.name, "error", err)
		}
		s.sess = nil
	}
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
func registerMCPServerTools(ctx context.Context, r *Registry, serverName string, ref *mcpServerSession, p OutputPolicy, registeredNames map[string]string) error {
	session, _, err := ref.current(ctx)
	if err != nil {
		return fmt.Errorf("mcp: listing tools for server %q: %w", serverName, err)
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcp: listing tools for server %q: %w", serverName, err)
	}

	normServer := normalizeMCPName(serverName)
	var spillCounter atomic.Int64
	for _, t := range res.Tools {
		name := fmt.Sprintf("mcp__%s__%s", normServer, normalizeMCPName(t.Name))
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
		adapter := &mcpAdapter{
			name:           name,
			description:    t.Description,
			rawSchema:      rawSchemaJSON(t.InputSchema),
			ref:            ref,
			toolName:       t.Name,
			registeredName: name,
			p:              p,
			spillCounter:   &spillCounter,
		}
		r.Register(adapter)
	}
	return nil
}

// mcpAdapter wraps an MCP tool as a Tool, so it can be registered on a
// Registry via Register().
type mcpAdapter struct {
	name           string
	description    string
	rawSchema      json.RawMessage
	ref            *mcpServerSession
	toolName       string
	registeredName string
	p              OutputPolicy
	spillCounter   *atomic.Int64
}

func (a *mcpAdapter) Name() string        { return a.name }
func (a *mcpAdapter) Description() string { return a.description }
func (a *mcpAdapter) InputSchema() Schema { return SchemaFromRaw(a.rawSchema) }

func (a *mcpAdapter) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var args any
	rawIn := json.RawMessage(input)
	if len(rawIn) > 0 {
		args = rawIn
	}

	res, err := a.ref.call(ctx, &mcp.CallToolParams{Name: a.toolName, Arguments: args})
	if err != nil {
		return ToolResult{}, fmt.Errorf("mcp: calling tool %q: %w", a.toolName, err)
	}
	if res == nil {
		return ToolResult{}, fmt.Errorf("mcp: tool %q returned no result", a.toolName)
	}

	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = fmt.Sprintf("%s_%d", a.registeredName, a.spillCounter.Add(1))
	}
	out := a.p.Clip(text.String(), spillName)

	if res.IsError {
		return ToolResult{}, fmt.Errorf("mcp: tool %q returned an error: %s", a.toolName, out)
	}
	return NewTextResult(out), nil
}

// rawSchemaJSON marshals an MCP tool's input schema to a JSON blob suitable
// for SchemaFromRaw. If the schema is nil, it returns an empty object.
func rawSchemaJSON(schema any) json.RawMessage {
	if schema == nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
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

// mcpServerEnv builds the environment for a stdio MCP server: the daemon's own,
// minus everything rafiki owns, plus the server's configured values.
//
// An MCP server is a third-party program an operator named in a config file to
// get one tool. Inheriting os.Environ() unfiltered handed it RAFIKI_DB — a
// connection string with credentials — along with RAFIKI_TOKEN and both
// provider API keys.
//
// cmd.Env is set UNCONDITIONALLY, and that is the point. A nil cmd.Env means
// "inherit the parent's environment", which is the unfiltered case this exists
// to prevent — and it was the original bug's exact shape, because the merge
// only ran when the config happened to set a variable of its own.
//
// Configured values are appended last so they win: exec resolves duplicates to
// the final occurrence, which lets an operator point one server at a different
// account without touching the daemon's environment.
func mcpServerEnv(base []string, extra map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if paths.IsReservedEnvKey(kv[:eq]) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}
