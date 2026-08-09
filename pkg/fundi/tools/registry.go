// Package tools implements fundi's native agent tool layer: a Registry that
// satisfies rafiki's agentloop.ToolSet, and the core file-manipulation tools
// (read, write, edit, glob, grep) the model calls through it.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	skillspkg "go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/tasks"
)

// ToolInput is the raw JSON input a tool receives from the model. It is a
// named type wrapping json.RawMessage so the tool interface is
// self-documenting.
type ToolInput json.RawMessage

// Unmarshal decodes this input into v using encoding/json.
func (i ToolInput) Unmarshal(v any) error {
	return json.Unmarshal(json.RawMessage(i), v)
}

// ContentBlock is one piece of a tool result. Today every result is text;
// the interface exists so a non-text block (an image, say) can be added
// without changing Tool, Registry, or any existing tool.
type ContentBlock interface{ isContentBlock() }

// TextBlock is a plain-text content block.
type TextBlock struct{ Text string }

func (TextBlock) isContentBlock() {}

// ToolResult is what a tool hands back to the model. Text carries the
// common case; Blocks is nil for every tool today and takes precedence
// over Text when set.
type ToolResult struct {
	Text   string
	Blocks []ContentBlock
}

// IsEmpty reports whether this result carries no text.
func (r ToolResult) IsEmpty() bool { return r.Text == "" }

// NewTextResult builds a ToolResult from a plain string.
func NewTextResult(s string) ToolResult { return ToolResult{Text: s} }

// NewErrorResult builds a ToolResult from an error value's message.
func NewErrorResult(err error) ToolResult {
	if err == nil {
		return ToolResult{}
	}
	return ToolResult{Text: err.Error()}
}

// ContentBlocks normalizes a result into blocks. A result with explicit
// Blocks yields those; otherwise a non-empty Text yields one TextBlock;
// an empty result yields none.
func (r ToolResult) ContentBlocks() []ContentBlock {
	if len(r.Blocks) > 0 {
		return r.Blocks
	}
	if r.Text == "" {
		return nil
	}
	return []ContentBlock{TextBlock{Text: r.Text}}
}

// Tool is the interface every agent tool implements. A tool value that
// implements the full interface can be registered on a Registry.
type Tool interface {
	Name() string
	Description() string
	InputSchema() Schema
	Execute(ctx context.Context, input ToolInput) (ToolResult, error)
}

// RTKMode controls whether the bash tool routes commands through rtk, a
// CLI proxy that compresses dev-command output. Phase 2 supplies the
// command mapping; this type is the switch.
type RTKMode string

const (
	// RTKAuto uses rtk when it is installed and the command maps to a
	// known rtk subcommand, and runs bash directly otherwise.
	RTKAuto RTKMode = "auto"
	// RTKOn behaves like RTKAuto but makes a missing rtk a startup error.
	RTKOn RTKMode = "on"
	// RTKOff never invokes rtk.
	RTKOff RTKMode = "off"
)

// ParseRTKMode maps a config value to a mode, defaulting to RTKAuto for
// empty or unrecognised input: an unreadable setting must not disable a
// tool outright.
func ParseRTKMode(s string) RTKMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on":
		return RTKOn
	case "off":
		return RTKOff
	default:
		return RTKAuto
	}
}

// LSPDiagnostic is a diagnostic message from a language server, translated
// from the LSP protocol into tool-agnostic fields.
type LSPDiagnostic struct {
	Path     string
	Line     int    // 0-based
	Column   int    // 0-based
	Severity string // "error", "warning", "info", "hint"
	Message  string
}

// LSPLocation is a file location returned by navigation operations.
type LSPLocation struct {
	URI  string
	Line int // 0-based
	Col  int // 0-based
	// Name and Kind are set for symbol results. Without them lsp_symbols
	// rendered every hit as a bare "  :3:6" — no name, no kind, and for
	// document symbols no path either — which made the tool useless in its
	// primary mode.
	Name string
	Kind string
}

// FileChangeNotifier is told when a tool has written to a file, so a
// language server's view of the workspace can be kept in sync.
//
// Without this the navigation tools answer from whatever the server last
// parsed: after the edit tool inserts lines, lsp_definition either errors
// with "line number N out of range" or — when the line count is unchanged,
// as in a rename or a constant change — returns a confidently wrong
// location with no error at all.
type FileChangeNotifier interface {
	NotifyChange(ctx context.Context, path string) error
}

// LSPCallHierarchyItem is a node in the call graph.
type LSPCallHierarchyItem struct {
	Name string
	URI  string
	Line int
	Col  int
}

// LSPClient is the interface the LSP tools use to talk to language servers.
// It is satisfied by *lsp.Manager from the lsp package, keeping the tools
// package free of direct LSP protocol types.
type LSPClient interface {
	// Diagnostics returns cached diagnostics for a file, or nil when no
	// language server handles it. The caller should first ensure the file
	// is open (DidOpen) so the server has processed it.
	Diagnostics(ctx context.Context, path string) ([]LSPDiagnostic, error)

	// DidOpen notifies the language server that a file has been opened.
	DidOpen(ctx context.Context, path, content string) error

	// DidChange notifies the server that a file's content changed.
	DidChange(ctx context.Context, path, content string) error

	// WaitForDiagnostics blocks until new diagnostics arrive after the
	// given start version, or the timeout expires.
	WaitForDiagnostics(ctx context.Context, path string, timeoutSec int) error

	// --- navigation ---

	// Definition returns the definition location(s) for the symbol at
	// the given 0-based line and column.
	Definition(ctx context.Context, path string, line, col int) ([]LSPLocation, error)

	// References returns all reference locations for the symbol.
	References(ctx context.Context, path string, line, col int) ([]LSPLocation, error)

	// DocumentSymbols returns the symbol outline for a file.
	DocumentSymbols(ctx context.Context, path string) ([]LSPLocation, error)

	// WorkspaceSymbols searches the workspace by name.
	WorkspaceSymbols(ctx context.Context, query string) ([]LSPLocation, error)

	// PrepareCallHierarchy resolves a call hierarchy item at the given position.
	PrepareCallHierarchy(ctx context.Context, path string, line, col int) ([]LSPCallHierarchyItem, error)

	// IncomingCalls returns callers of the given call hierarchy item.
	IncomingCalls(ctx context.Context, item LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error)

	// OutgoingCalls returns callees of the given call hierarchy item.
	OutgoingCalls(ctx context.Context, item LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error)

	// --- mutation ---

	// Rename renames the symbol at the given position across the workspace.
	// It returns the list of files that were modified.
	Rename(ctx context.Context, path string, line, col int, newName string) ([]string, error)

	// Restart restarts the language server for the given path.
	Restart(ctx context.Context, path string) error
}

// ToolOpts carries the per-agent runtime state a Materializer needs to
// build a concrete Tool from a blueprint that carries only its static
// metadata (name, description, input schema).
type ToolOpts struct {
	Cwd          string
	FileTracker  *FileTracker
	OutputPolicy OutputPolicy
	Skills       []skillspkg.SkillMeta
	RTK          RTKMode
	Web          bool

	// HTTPClient, when non-nil, is used for outbound HTTP requests.
	// nil means the tool creates its own client with SSRF protection.
	// Set by tests that provide an httptest server on loopback.
	HTTPClient *http.Client

	// LSP, when non-nil, provides access to language server diagnostics
	// and document sync. nil means LSP tools decline to materialize.
	LSP LSPClient

	// FileChanged, when non-nil, is notified after edit/write modify a
	// file so a language server can re-read it. nil disables the
	// notification, which is correct when no LSP server is configured.
	FileChanged FileChangeNotifier

	// Pool, when non-nil, is the shared database pool backing per-conversation
	// tool state persistence. nil means in-memory-only (tests, standalone).
	Pool *pgxpool.Pool

	// Tasks is the task ledger backing the task_* tools. It is NEVER nil once
	// those tools exist: BuildRuntime supplies an in-memory store when no pool
	// is configured, so a DB-less agent keeps working with state lost on
	// restart. This deliberately does NOT follow LSP's nil-means-decline
	// pattern, which would silently remove task tracking from every DB-less
	// agent and from every test constructing a bare ToolOpts{}.
	Tasks tasks.Store

	// ChildID is this agent's own child id (RuntimeOptions.Ref).
	//
	// Unlike the conversation ID — which does not exist until NewEngine runs,
	// after materialization, and so must travel by context — the childID is
	// already known here. That is what lets a task tool resolve "my assigned
	// task" from assignee = me with no spawn-time handshake.
	ChildID string

	// Executor, when non-nil, runs the filesystem and shell tools in a
	// separate process rather than in-process. nil keeps every tool local,
	// which is the default and preserves today's behaviour exactly.
	Executor ExecutorClient
}

// ConversationIDKey is the context key for the conversation ID injected by the
// engine before each tool execution. Tools that need per-conversation persistence
// read it from ctx via ConversationIDFromContext. It is exported so the engine
// wrapper (pkg/fundi) can set it without importing this package.
type ConversationIDKey struct{}

// ConversationIDFromContext extracts the conversation ID set by the engine, or
// returns "" when none was set (in-memory conversation, tests).
func ConversationIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(ConversationIDKey{}).(string); ok {
		return id
	}
	return ""
}

// Materializer is an optional extension of Tool that a blueprint implements
// when its concrete Execute needs runtime state. Blueprints without runtime
// state (glob, grep) are their own concrete tools and do not implement
// Materializer.
//
// Returning (nil, nil) declines the tool for these opts: MaterializeAll leaves
// it out of the Registry entirely. Use that instead of returning a tool that
// can only fail when opts lack what it needs.
type Materializer interface {
	Materialize(opts ToolOpts) (Tool, error)
}

// Registry is a name -> (definition, executor) map implementing rafiki's
// agentloop.ToolSet. The loop runs a tool batch concurrently (ergroup,
// concurrency limit 6), so every Registry method is safe to call from
// multiple goroutines at once.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]anthropic.ToolUnionParam
	fns  map[string]func(context.Context, json.RawMessage) (string, error)
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		defs: make(map[string]anthropic.ToolUnionParam),
		fns:  make(map[string]func(context.Context, json.RawMessage) (string, error)),
	}
}

// Register adds a Tool to the registry. The tool's Anthropic definition is
// built from its Name(), Description(), and InputSchema().JSON().
func (r *Registry) Register(t Tool) {
	def := BuildDef(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.OfTool.Name] = def
	r.fns[def.OfTool.Name] = func(ctx context.Context, input json.RawMessage) (string, error) {
		result, err := t.Execute(ctx, ToolInput(input))
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, b := range result.ContentBlocks() {
			if tb, ok := b.(TextBlock); ok {
				sb.WriteString(tb.Text)
			}
		}
		return sb.String(), nil
	}
}

// Definitions returns every registered tool's definition, sorted by name.
// The sort is load-bearing, not cosmetic: Definitions backs the tools array
// sent on every turn, and a reordered array silently busts the Anthropic
// prompt-cache prefix (see rafiki's agentloop.ToolSet).
func (r *Registry) Definitions() []anthropic.ToolUnionParam {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.defs))
	for name := range r.defs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]anthropic.ToolUnionParam, 0, len(names))
	for _, name := range names {
		out = append(out, r.defs[name])
	}
	return out
}

// Execute runs the named tool. An unknown name is a returned error, not a
// panic — agentloop converts it to an is_error tool result the model can see.
//
// A panic inside a tool's Execute is recovered here and converted into that
// same returned error. This is the containment boundary for the ENTIRE tool
// surface — the file tools, bash, skill, and every MCP tool — and it is the
// only place that containment can live: agentloop runs each tool call on its
// own errgroup goroutine (one g.Go per tool_use block) and errgroup
// deliberately does not recover. Without this, a panic in any tool body
// unwinds a goroutine nothing owns and kills the whole daemon, taking every
// unrelated conversation with it.
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (result string, err error) {
	r.mu.RLock()
	fn, ok := r.fns[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	defer func() {
		if v := recover(); v != nil {
			slog.Error("tools: tool panicked; reporting a failed tool result to the model",
				"tool", name, "panic", v, "stack", string(debug.Stack()))
			result = ""
			err = fmt.Errorf("tool %q panicked: %v", name, v)
		}
	}()
	return fn(ctx, input)
}

// notifyFileChanged tells a language server that path was rewritten.
//
// Best-effort by design: the write already succeeded, so a sync failure must
// not turn a successful edit into a tool error. It is logged rather than
// swallowed silently so a server that has stopped accepting notifications is
// still diagnosable.
func notifyFileChanged(ctx context.Context, n FileChangeNotifier, path string) {
	if n == nil {
		return
	}
	if err := n.NotifyChange(ctx, path); err != nil {
		slog.Debug("tools: lsp change notification failed", "path", path, "err", err)
	}
}
