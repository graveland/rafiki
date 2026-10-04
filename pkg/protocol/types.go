// Package protocol defines the typed data shapes rafiki's control plane and
// executor/daraja links exchange. This is a pure-data package: no logic, no
// I/O. The Connect control plane maps these shapes onto its generated
// protobuf types (pkg/gen/rafiki/v1).
//
// Cross-references (historical spec section numbers retained by the field
// comments):
//
//	§8  — error codes
//	§10 — status constants
package protocol

import "encoding/json"

// ─── Status constants (§10) ──────────────────────────────────────────────────

// Status is the state of a pi child process.
type Status string

const (
	StatusSpawning  Status = "spawning"
	StatusIdle      Status = "idle"
	StatusStreaming Status = "streaming"
	// StatusRunning is a script child whose process is alive; scripts have
	// no turns, so they never report idle/streaming. Set once at spawn for
	// kind=script, never touched by output, replaced only by exited. It is a
	// working status for every consumer that asks "is it working".
	StatusRunning      Status = "running"
	StatusToolRunning  Status = "tool_running"
	StatusCompacting   Status = "compacting"
	StatusBatchWait    Status = "batch_wait"
	StatusBlockedUI    Status = "blocked_ui"
	StatusShuttingDown Status = "shutting_down"
	StatusExited       Status = "exited"
)

// ─── Error code constants (§8) ───────────────────────────────────────────────

// The ErrorInfo reason vocabulary. A daemon error carries one of these as its
// google.rpc.ErrorInfo reason (see pkg/rpcreason), and every Connect code is
// derived from it (pkg/connectapi's errCodeTable).
const (
	// ErrChildNotFound is returned when no child with the given childId exists.
	ErrChildNotFound = "child_not_found"
	// ErrChildExited is returned when the child has already exited.
	ErrChildExited = "child_exited"
	// ErrChildInGrace is equivalent to ErrChildExited; explicit for clarity.
	ErrChildInGrace = "child_in_grace"
	// ErrChildShuttingDown is returned when stdin is closed during graceful shutdown.
	ErrChildShuttingDown = "child_shutting_down"
	// ErrNotResumable is returned by Resume when the child is not in exited status.
	ErrNotResumable = "not_resumable"
	// ErrNotExited is returned by Close when the child is still live.
	ErrNotExited = "not_exited"
	// ErrSessionFileMissing is returned by Resume when the session file is gone.
	ErrSessionFileMissing = "session_file_missing"
	// ErrBackpressure is returned when the child's command channel is full.
	ErrBackpressure = "backpressure"
	// ErrAtCapacity is a spawn refused because the parent's subtree is at
	// its live-children cap: transient — retry once a child settles.
	ErrAtCapacity = "at_capacity"
	// ErrInvalidArgs is returned when request fields fail validation.
	ErrInvalidArgs = "invalid_args"
	// ErrSpawnFailed is returned when the child subprocess fails to start.
	ErrSpawnFailed = "spawn_failed"
	// ErrAuthRequired is returned when a caller presents no credential where
	// one is required.
	ErrAuthRequired = "auth_required"
	// ErrAuthInvalid is returned when the credential presented does not resolve.
	ErrAuthInvalid = "auth_invalid"
	// ErrNotFound is the generic not-found error.
	ErrNotFound = "not_found"
	// ErrPermissionDenied is returned when an authenticated caller addresses
	// a resource that belongs to a different user (executor ownership and
	// the like): the request is well-formed, the target is simply not
	// theirs. Distinct from the credential-shaped refusals (auth_required /
	// auth_invalid) and from not_found, which would leak existence.
	ErrPermissionDenied = "permission_denied"
	// ErrInternal is returned on unexpected controller-side errors.
	ErrInternal = "internal"
	// ErrNoAgentDB is returned by the conversation-insight queries when the
	// daemon has no agent database configured (RAFIKI_DB unset).
	ErrNoAgentDB = "no_agent_db"
	// ErrPayloadTooLarge is returned when a marshaled response would exceed
	// the response size budget.
	ErrPayloadTooLarge = "payload_too_large"
)

// ─── Shared sub-shapes ───────────────────────────────────────────────────────

// ListFilter narrows list results. All fields are optional (§6.1).
// Labels is an AND-match: every key=value pair must be present on the child.
// HasLabel matches children that have the key present regardless of value.
type ListFilter struct {
	Status       string            `json:"status,omitempty"`
	Name         string            `json:"name,omitempty"`
	NameContains string            `json:"nameContains,omitempty"`
	CwdContains  string            `json:"cwdContains,omitempty"`
	Since        int64             `json:"since,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`   // AND-match: all k=v must match
	HasLabel     []string          `json:"hasLabel,omitempty"` // key presence only
}

// SearchSessionFilter narrows which children a content search scans (§6.15).
// Labels/HasLabel apply the same AND-match semantics as ListFilter.
type SearchSessionFilter struct {
	CwdContains  string            `json:"cwdContains,omitempty"`
	NameContains string            `json:"nameContains,omitempty"`
	Since        int64             `json:"since,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	HasLabel     []string          `json:"hasLabel,omitempty"`
}

// ─── Requests ────────────────────────────────────────────────────────────────

// PrefillRead is one entry of a spawn's pre-fill: a file (or glob) the
// child reads through its own Read tool before turn 1. Start/End are
// 1-based inclusive line numbers; 0 means open (from the start / to EOF).
type PrefillRead struct {
	Path  string `json:"path"`
	Start int    `json:"start,omitempty"`
	End   int    `json:"end,omitempty"`
}

// StepSite names whose authority, executor and working directory a send
// step runs under. The empty string is refused, never defaulted: a
// zero-valued SendStep must not silently run anywhere.
type StepSite string

const (
	// StepSiteChild runs the step on the TARGET child's executor, in its
	// workspace and working directory.
	StepSiteChild StepSite = "child"
	// StepSiteSender runs the step on the CALLER's executor, under the
	// caller's own tool allowlist; only the output reaches the target.
	StepSiteSender StepSite = "sender"
)

// SendStep is one tool call the daemon runs at send time, whose rendered
// output is appended to the message. Exactly one of Read, Bash, PymoduleRun
// is set.
type SendStep struct {
	Where       StepSite         `json:"where"`
	Echo        bool             `json:"echo,omitempty"`
	Read        *PrefillRead     `json:"read,omitempty"`
	Bash        *BashStep        `json:"bash,omitempty"`
	PymoduleRun *PymoduleRunStep `json:"pymodule_run,omitempty"`
}

// BashStep runs one shell command. TimeoutMs 0 means the send-step default.
type BashStep struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// PymoduleRunStep runs a saved pymodule, with pymodule_run's inputs.
type PymoduleRunStep struct {
	Repo    string   `json:"repo"`
	Script  string   `json:"script"`
	Modules []string `json:"modules,omitempty"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

// StepSummary is what the sender learns about one step: never its output,
// except the Echo prefix when the step asked for it.
type StepSummary struct {
	Index     int      `json:"index"`
	Tool      string   `json:"tool"`
	Where     StepSite `json:"where"`
	Outcome   string   `json:"outcome"`
	Bytes     int      `json:"bytes"`
	Truncated bool     `json:"truncated,omitempty"`
	Echo      string   `json:"echo,omitempty"`
}

// ScriptSpec, for kind=script, names the pymodule that IS the child's brain:
// a saved Python module run as the child's process, with a per-child Connect
// socket as its control channel (RAFIKI_CHILD_CONNECT). It is a pointer so
// "absent" stays distinguishable from a spec with empty fields; the daemon
// refuses a script spawn without one.
//
// Repo selects the pymodule source: "local" (pymodules.LocalRepo) for the
// owner's own saved modules, or a registered git source's name. Script is the
// entry module, a bare Python identifier exactly as saved. Modules names
// further pymodules the script imports, resolved within the same repo and
// put on PYTHONPATH for the run. Args are extra command-line arguments passed
// to the script.
type ScriptSpec struct {
	Repo    string   `json:"repo"`
	Script  string   `json:"script"`
	Modules []string `json:"modules,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// SpawnRequest starts a new child (§6.3).
// cwd is required; all other fields are optional and forwarded to the child
// as flags. apiKey is used at spawn time only and is never written to the
// state record.
//
// This is the daemon's canonical spawn shape and the client's assembly
// struct: the Connect Spawn adapter (cmd/rafiki's connectSpawnRequest)
// converts it field for field onto the wire, and the daemon's Connect
// handler rebuilds one from the wire request before Controller.Spawn sees it.
type SpawnRequest struct {
	// Kind selects the child protocol/binary: "fundi" (default, when empty —
	// this repo's own agent runtime, run in-process), "claude" (Claude Code
	// CLI, driven over stream-json), or "script" (a saved pymodule run as a
	// process; ScriptSpec names it). See kinds.go; "pi" was a third kind and
	// was retired, with no alias.
	Kind string `json:"kind,omitempty"`

	// Script, for kind=script, names the pymodule the child runs. The
	// fundi/claude-only fields (model, tools, skills, MCP, prompts, sessions)
	// are refused on a script spawn, and Prefill with them — a script has no
	// engine to pre-fill and no model to think with. See ScriptSpec above.
	Script *ScriptSpec `json:"script,omitempty"`

	// Preset names a daemon-side preset (conversations.presets) that
	// Controller.Spawn resolves FIRST, before any other field is read: it
	// supplies kind, model, tools, prompt and budgets, which the remaining
	// fields then override or narrow. Empty means no preset.
	Preset string `json:"preset,omitempty"`

	// Prefill lists files the child reads through its own Read tool, on its
	// own executor, before its first turn; they're recorded as real
	// tool_use/tool_result history. fundi only. Empty means none.
	Prefill []PrefillRead `json:"prefill,omitempty"`

	// ConfigDir, for kind=claude, is exported to the child as CLAUDE_CONFIG_DIR
	// — it selects the claude config dir (plugins, hooks, MCP, settings). It is
	// persisted so a resumed claude child re-uses the same profile.
	ConfigDir string `json:"configDir,omitempty"`

	// Identity
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"` // user-supplied labels; rafiki/ prefix rejected

	// ParentChildID names the child spawning this one, forming the tree edge
	// recorded as the rafiki/parent label (rafiki/root is derived from it).
	// Empty means top-level.
	//
	// From an ordinary client connection this is honoured as given — the
	// caller is already authenticated to the daemon. When a spawn originates
	// from an agent's own spawn tool, the controller overrides it with the
	// calling child's real id, so an agent cannot claim a parent it does not
	// have. The lineage labels themselves are never accepted from a caller:
	// the rafiki/ prefix is rejected in Labels above.
	ParentChildID string `json:"parentChildId,omitempty"`

	// Task is a handle ("2.1") in the SPAWNER's task ledger to assign to the
	// new child. Honoured only when ParentChildID is set, because a handle is
	// meaningless without a conversation to resolve it against.
	//
	// The assignment is written only after the controller has admitted the
	// spawn. There is deliberately no task_delegate verb: it would clone this
	// whole struct's surface to add one field, and a separate assign call
	// leaves a window where a row points at a child that does not exist.
	Task string `json:"task,omitempty"`

	// SpawnerConversationID names the conversation whose ledger Task resolves
	// against. Set by the daemon from the spawning child's own record, never
	// by a client: a handle is relative to a conversation, and resolving it
	// against the wrong one silently assigns somebody else's row.
	SpawnerConversationID string `json:"spawnerConversationId,omitempty"`

	// Working directory (required, absolute).
	Cwd string `json:"cwd"`

	// Model + auth (the runtime resolves from its own config when omitted).
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"` // off|low|medium|high|xhigh
	APIKey   string `json:"apiKey,omitempty"`

	// Routing is the resolved canonical routing spec (routing.Spec.String(),
	// e.g. "sort=price,nodata") this spawn runs under: spawn spec merged over
	// preset spec over routing-policy row. The DAEMON sets it, once, in
	// Controller.Spawn — a caller never does (a caller expresses routing
	// inside Model, as a bracketed spec, which Spawn parses and strips into
	// Model's base id plus this field). Resume keeps the stored value
	// verbatim: routing resolves once, like the preset. Empty = no spec.
	Routing string `json:"routing,omitempty"`

	// Session flags.
	NoSession     bool   `json:"noSession,omitempty"`
	SessionDir    string `json:"sessionDir,omitempty"`
	ResumeSession string `json:"resumeSession,omitempty"`
	ForkSession   string `json:"forkSession,omitempty"`

	// Tool / extension / skill scoping.
	Tools          string   `json:"tools,omitempty"` // comma-joined
	NoTools        bool     `json:"noTools,omitempty"`
	NoBuiltinTools bool     `json:"noBuiltinTools,omitempty"`
	Extensions     []string `json:"extensions,omitempty"`
	NoExtensions   bool     `json:"noExtensions,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	NoSkills       bool     `json:"noSkills,omitempty"`
	// SkillsDirs are additional skill directories for an agent-kind child,
	// appended after the configured and project dirs (highest precedence).
	SkillsDirs []string `json:"skillsDirs,omitempty"`
	// MCPConfig overrides the .mcp.json path for an agent-kind child.
	MCPConfig string `json:"mcpConfig,omitempty"`
	// MCPServers is an allowlist of MCPConfig's mcpServers keys to connect;
	// empty means all. The entries are comma-joined into a single
	// --mcp-servers flag by buildAgentArgv — unlike SkillsDirs, which renders
	// as one repeated flag per entry. NoMCP disables MCP entirely, even when
	// MCPConfig is set. Both are agent-kind (fundi) only, same as MCPConfig
	// itself.
	MCPServers        []string `json:"mcpServers,omitempty"`
	NoMCP             bool     `json:"noMcp,omitempty"`
	PromptTemplates   []string `json:"promptTemplates,omitempty"`
	NoPromptTemplates bool     `json:"noPromptTemplates,omitempty"`
	Themes            []string `json:"themes,omitempty"`
	NoThemes          bool     `json:"noThemes,omitempty"`
	NoContextFiles    bool     `json:"noContextFiles,omitempty"`

	// System prompt.
	SystemPrompt       string `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string `json:"appendSystemPrompt,omitempty"`

	// Verbosity.
	Verbose bool `json:"verbose,omitempty"`

	// Process control.
	PiBinary    string            `json:"piBinary,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvOverride bool              `json:"envOverride,omitempty"`

	// Escape hatch: appended last to argv, wins by last-flag-wins.
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// ResumedFromSession is set when spawning a fresh child to continue a
	// session file that was not previously managed by rafiki. When non-empty
	// the daemon adds the reserved auto-label
	// `rafiki/resumed-from-session=<value>` to the new child. Sent here (rather
	// than in Labels) because the `rafiki/` namespace is reserved for daemon
	// auto-labels; user-supplied Labels with that prefix are rejected.
	ResumedFromSession string `json:"resumedFromSession,omitempty"`

	// RecordRequests, when true, records raw LLM API request/response pairs
	// to the debug raw_http_request hypertable (agent-kind only; requires
	// RAFIKI_RECORD_REQUESTS=1 at daemon startup).
	RecordRequests bool `json:"recordRequests,omitempty"`

	// PassthroughAuth is kind=claude's tri-state billing choice: ""/"auto"
	// bills the user's own Claude subscription when Model resolves to an
	// Anthropic id (proxyenv.AnthropicModel) and the daemon's key otherwise,
	// "on" always bills the subscription, "off" always bills the daemon's
	// key. Mirrors `rafiki claude --passthrough-auth` exactly
	// (proxyenv.ParsePassthroughMode/PassthroughAuthFor parse and resolve
	// both) — only meaningful for a daraja-routed claude child; the
	// local-subprocess claude path has no passthrough support at all (see
	// proxyChildEnv's own doc comment for why).
	PassthroughAuth string `json:"passthroughAuth,omitempty"`

	// ExecutorSelector is a label selector for picking an executor from the
	// live pool — the path the daemon can audit.
	ExecutorSelector string `json:"executorSelector,omitempty"`

	// ExecutorRef pins a spawn to ONE specific executor by its human-readable
	// machine label (e.g. "greyshift") or its raw id, resolved against the
	// SAME narrowed candidate set ExecutorSelector would produce — an
	// explicit ref bypasses SEARCH, never confinement. Mutually exclusive
	// with ExecutorSelector at the CLI layer; the daemon does not enforce
	// that itself, since a hand-built request naming both is simply resolved
	// by ref first (see chooseExecutor/chooseLaunchExecutor).
	ExecutorRef string `json:"executorRef,omitempty"`

	// WorkspaceMode selects how the child's workspace is provisioned:
	// "ephemeral" (reschedulable) or "pinned" (existing tree).
	WorkspaceMode string `json:"workspaceMode,omitempty"`

	// SkipDerivedIndex, when true, skips embedding and summarising this child's
	// conversations and every descendant's; windows and BM25 search still work.
	// Child-allowed: it only reduces spend.
	SkipDerivedIndex bool `json:"skipDerivedIndex,omitempty"`

	// ─── Resource grants (phase 05) ───
	//
	// All three are POINTERS so "unset" is distinguishable from "zero". The
	// distinction is load-bearing in opposite directions for each: an unset
	// MaxDepth means the default 1, a zero means "this child may not spawn";
	// an unset MaxCost means unlimited, a zero means "spend nothing".
	// Collapsing either to a plain int silently converts one into the other.

	// MaxDepth is how many further levels of descendants the NEW child may
	// create. 0 means it cannot spawn. Default 1 when unset.
	//
	// It does NOT decrement: a parent grants what its child needs without
	// reference to its own allowance. The safety bound is RAFIKI_MAX_DEPTH,
	// an absolute ceiling on the child's position in the tree that the daemon
	// computes from stored lineage labels.
	MaxDepth *int `json:"maxDepth,omitempty"`

	// MaxCost is the new child's subtree budget in USD. Unset means
	// unlimited — the right default for a top-level interactive agent and the
	// wrong one for a coordinator, which should always set it. A child may be
	// granted at most its parent's REMAINING budget; unlike depth, this one
	// decrements across the subtree.
	MaxCost *float64 `json:"maxCost,omitempty"`

	// MaxChildren caps simultaneously LIVE descendants across the new child's
	// subtree. Default 10. It is separate from cost because a runaway
	// recursion of cheap spawns exhausts the machine long before it exhausts
	// a dollar budget.
	MaxChildren *int `json:"maxChildren,omitempty"`
}

// TaskListRequest queries the task ledger (§ TaskList).
type TaskListRequest struct {
	// ConversationID scopes the query to one conversation's ledger. Empty
	// means every conversation, matching tasks.ListFilter.
	ConversationID string `json:"conversationId,omitempty"`
	ChildID        string `json:"childId,omitempty"` // tasks assigned to this child
	Status         string `json:"status,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	All            bool   `json:"all,omitempty"` // include dropped
}

// ─── Response data types ─────────────────────────────────────────────────────

// ChildSummary is a single entry in a list / get response (§6.1).
// PID is nil when status is exited. ExitCode is nil while the child is alive.
// ExitSignal is absent (not "null") when the child exited via normal exit code rather than a signal.
type ChildSummary struct {
	ChildID       string            `json:"childId"`
	PID           *int              `json:"pid"` // null when exited
	Cwd           string            `json:"cwd"`
	Name          string            `json:"name,omitempty"`
	Kind          string            `json:"kind,omitempty"` // child protocol kind ("claude"); absent for pi children
	Model         string            `json:"model,omitempty"`
	SessionID     string            `json:"sessionId,omitempty"`
	SessionFile   string            `json:"sessionFile,omitempty"`
	Status        string            `json:"status"`
	StartedAt     int64             `json:"startedAt"`
	LastActivity  int64             `json:"lastActivity"`
	ExitCode      *int              `json:"exitCode"` // null while alive
	ExitSignal    string            `json:"exitSignal,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	SlashCommands []string          `json:"slashCommands,omitempty"`
	// ContextWindow/MaxCompletionTokens are the daemon's own model catalog's
	// answer for Model (see routing.ModelCatalog.ContextWindow), independent
	// of whatever static model list a client-side TUI might carry. Omitted
	// (both zero) when the catalog has no entry for Model — an unconfigured
	// catalog, a model the catalog hasn't seen, or a cold/stale cache.
	ContextWindow       int `json:"contextWindow,omitempty"`
	MaxCompletionTokens int `json:"maxCompletionTokens,omitempty"`
	// CostUSD is total spend, or nil when it could not be determined. Pointer
	// because 0 means "spent nothing" and nil means "not known" -- with no
	// database configured there is no rollup to read.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// MaxCost is the child's spend cap, or nil when it has none. Unlike
	// CostUSD, nil here is the SAME as "no cap" (childstore.Session.MaxCost
	// treats its own zero value as unlimited -- see grantedCost), so this is
	// only ever set when the underlying cap is a real positive number.
	MaxCost *float64 `json:"max_cost,omitempty"`
	// Result is the script child's final result (Connect SetResult): verbatim
	// JSON, omitted when the child has not set one. See childstore.Session.Result.
	Result string `json:"result,omitempty"`
	// Routing is the child's resolved routing spec (routing.Spec.String()),
	// mirrored from childstore.Session.Routing — the same value the daemon
	// recorded on the session at spawn. Empty when the spawn carried and
	// resolved to no spec. Session.Model, unlike this field, is the base id:
	// the spec lives only here and in the rafiki/routing label.
	Routing string `json:"routing,omitempty"`
}

// SpawnResponseData is the data payload of a Spawn/Resume answer (§6.3).
type SpawnResponseData struct {
	ChildID     string `json:"childId"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionFile string `json:"sessionFile,omitempty"`
	Model       string `json:"model,omitempty"`
}

// KillResponseData is the data payload of a Kill answer (§6.5).
// ExitCode is nil when the child was killed by signal with no exit code.
// Signal is absent (not "null") when the child exited via normal exit code rather than a signal.
// Escalated is true if SIGTERM or SIGKILL was needed.
// Abandoned is true when even SIGKILL/forced teardown never produced a reap and
// the daemon gave up waiting, leaking the child's execution context (see
// internal/child's abandonTimeout). Omitted when false, so a reaped kill's
// payload is unchanged.
type KillResponseData struct {
	ExitCode   *int   `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Escalated  bool   `json:"escalated"`
	Abandoned  bool   `json:"abandoned,omitempty"`
}

// GetRecentResponseData is the data payload of a GetRecent answer (§6.11).
// Each element of Events is a verbatim event in publish order.
type GetRecentResponseData struct {
	Events           []json.RawMessage `json:"events"`
	TotalInBuffer    int               `json:"totalInBuffer"`
	OldestTimestamp  int64             `json:"oldestTimestamp"`
	TruncatedByLimit bool              `json:"truncatedByLimit"`
	// TruncatedBySize reports that oldest events were dropped so the response
	// stays under the MaxFrameBytes reader cap.
	TruncatedBySize bool `json:"truncatedBySize,omitempty"`
}

// GetStreamsResponseData carries raw, uncompressed stream bytes for a live
// child. In holds stdin frames (one []byte per frame, no trailing newline).
// Alive is false when the child has already exited, signalling the caller to
// fall back to the on-disk dump.
//
// Err is always nil for a live child by design: the stderr buffer is unguarded
// and racing the reader goroutine, so live stderr is never served. Stderr is
// only available post-exit via the on-disk dump. The field remains in the
// payload for forward compatibility but is never populated by this RPC.
type GetStreamsResponseData struct {
	Alive bool     `json:"alive"`
	In    [][]byte `json:"in,omitempty"`
	Err   []byte   `json:"err,omitempty"`
}

// SearchHit is one content match in a Search response (§6.15).
type SearchHit struct {
	ChildID     string `json:"childId"`
	SessionFile string `json:"sessionFile"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionName string `json:"sessionName,omitempty"`
	EntryID     string `json:"entryId,omitempty"`
	Timestamp   int64  `json:"timestamp"`
	Role        string `json:"role,omitempty"`
	Snippet     string `json:"snippet"`
	MatchStart  int    `json:"matchStart"`
	MatchEnd    int    `json:"matchEnd"`
}

// SearchResponseData is the data payload of a Search answer (§6.15).
type SearchResponseData struct {
	Hits      []SearchHit `json:"hits"`
	TotalHits int         `json:"totalHits"`
	Scanned   int         `json:"scanned"`
	Elapsed   int64       `json:"elapsed"`
}

// ChildCounts breaks down live vs exited child totals for StatusResponseData.
type ChildCounts struct {
	Live   int `json:"live"`
	Exited int `json:"exited"`
}

// StatusResponseData is the data payload of a Status answer (§6.16).
type StatusResponseData struct {
	Version     string      `json:"version"`
	StartedAt   int64       `json:"startedAt"`
	Children    ChildCounts `json:"children"`
	MemoryBytes int64       `json:"memoryBytes"`
	Socket      string      `json:"socket,omitempty"`
	LogsDir     string      `json:"logsDir,omitempty"`
}

// ─── Models ──────────────────────────────────────────────────────────────────

// ModelInfo is one entry in a ListModels answer.
type ModelInfo struct {
	ID       string `json:"id"` // "provider/model"
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Name     string `json:"name,omitempty"` // display name from models.json
	Source   string `json:"source"`         // user-config | builtin | ollama | lmstudio
}

// ModelInfoResponseData answers ModelInfo.
//
// Known == false means "the daemon has no entry for this model" and is an
// ordinary answer, not an error: every caller degrades by leaving the model's
// own defaults alone, and making it an error would force each of them to
// distinguish "unknown model" from "daemon unreachable" when the handling is
// identical.
//
// AutoCompactWindow is computed HERE, not by the caller. The formula would
// otherwise live in two binaries that must agree with nothing enforcing it —
// the drift class this repo already carries three documented instances of.
type ModelInfoResponseData struct {
	Model               string `json:"model"`
	ResolvedID          string `json:"resolvedId,omitempty"`
	ContextWindow       int    `json:"contextWindow"`
	MaxCompletionTokens int    `json:"maxCompletionTokens"`
	AutoCompactWindow   int    `json:"autoCompactWindow"`
	Known               bool   `json:"known"`
}

// ─── Executor management ─────────────────────────────────────────────────────

// ExecutorEnrollRequest mints a one-time enrollment token.
type ExecutorEnrollRequest struct {
	// Name is the name of the MACHINE the executor will run on, which is not
	// necessarily the one that minted the token: the daemon writes it to the
	// `machine` trust label. It cannot be sent as a label — see Labels.
	Name string `json:"name,omitempty"`
	// Labels are the operator's own trust labels. `owner` and `machine` are
	// written by the daemon (from the connection and from Name); a request
	// carrying either key is REFUSED rather than silently overwritten.
	Labels        map[string]string `json:"labels,omitempty"`
	Roots         []string          `json:"roots,omitempty"`
	Isolation     string            `json:"isolation,omitempty"`
	WorkspaceMode string            `json:"workspaceMode,omitempty"`
	Admits        string            `json:"admits,omitempty"`
	TTLSeconds    int64             `json:"ttlSeconds"`
}

// ExecutorEnrollResponseData is the data payload of an enroll answer.
type ExecutorEnrollResponseData struct {
	Token string `json:"token"`
}

// ExecutorCreateRequest mints an executor row and its durable credential in one
// step, with no enrollment handshake.
//
// This is the STATELESS path. An enrolled executor persists the credential it
// was issued, and one that loses that file cannot rejoin — its enrollment token
// was consumed — which makes enrollment awkward for a deployment with no durable
// local storage. Here the operator receives the credential and injects it from a
// secret store instead.
//
// The trade runs the other way from enrollment: the operator handles a
// long-lived secret, and a theft is silent rather than announcing itself by
// consuming a one-time token. Prefer enrollment where the machine can keep a
// file.
type ExecutorCreateRequest struct {
	// Name names the machine this executor runs on; the daemon writes it to
	// the `machine` trust label. Same rule as ExecutorEnrollRequest.Name.
	Name string `json:"name,omitempty"`
	// Labels are the operator's own trust labels. `owner` and `machine` are
	// daemon-written; a request carrying either key is REFUSED.
	Labels        map[string]string `json:"labels,omitempty"`
	Roots         []string          `json:"roots,omitempty"`
	Isolation     string            `json:"isolation,omitempty"`
	WorkspaceMode string            `json:"workspaceMode,omitempty"`
	Admits        string            `json:"admits,omitempty"`
}

// ExecutorCreateResponseData carries the new row's id and its credential. The
// credential is shown ONCE; only its hash is stored.
type ExecutorCreateResponseData struct {
	ExecutorID string `json:"executorId"`
	Credential string `json:"credential"`
}

// ExecutorSessionRequest asks the daemon for an executor row belonging to the
// caller's own machine, so the client can serve its operator's filesystem as a
// workspace.
//
// It carries only fields that do NOT gate access. owner, isolation,
// workspace_mode and admits are all decided by the daemon from the connection,
// because a client that names them can grant itself anything — the same reason
// the executor's self-reported capability facts are kept out of the trust
// labels.
type ExecutorSessionRequest struct {
	// Name is the operator-chosen name of the client's machine, so the daemon
	// can find a durable executor that shares this filesystem.
	//
	// It does NOT gate access — owner and admits are still derived from the
	// connection — so a client naming it can only ever narrow which of its OWN
	// executors it reaches. Matched against the `machine` trust label, which an
	// operator wrote at mint time; never against SelfReported, which is the
	// executor's own account of itself.
	Name string `json:"name,omitempty"`

	// Roots describes the directories this machine offers, for humans and for
	// selectors. Nothing enforces them and nothing may imply it does — a
	// native executor has no path scoping by design.
	Roots []string `json:"roots,omitempty"`
}

// ExecutorSessionResponseData answers it.
//
// Three outcomes, and Credential alone cannot express them — which is why
// RunLocal is a separate field rather than inferred from an empty credential:
//
//	RunLocal=false             a durable executor already covers this name and
//	                           owner. ExecutorID names it and the client starts
//	                           nothing. That executor outlives the client, which
//	                           is what keeps an agent working after the operator
//	                           detaches.
//	RunLocal=true, no cred     the client already holds a credential. Start an
//	                           executor and connect with the one it has.
//	RunLocal=true, with cred   a new row was minted. Persist the credential and
//	                           connect.
type ExecutorSessionResponseData struct {
	// ExecutorID names the row — minted or existing — that represents this
	// machine. The daemon uses it to route spawns from this client implicitly,
	// so the client never needs a label selector for "myself".
	ExecutorID string `json:"executorId"`

	// RunLocal says whether the client should serve an executor at all.
	RunLocal bool `json:"runLocal,omitempty"`

	// Ticket authenticates the transient executor this client should now
	// start. One-shot, and revoked when the session that asked for it ends.
	// Empty when RunLocal is false.
	Ticket string `json:"ticket,omitempty"`

	// Selector is the label selector the client should put on a spawn to land
	// it on this row. The client cannot build it itself: it does not know the
	// owner, which is derived here from the connection. For a client row it is
	// owner=<owner>,kind=client (unique because only session minting writes
	// kind=client); for a durable executor it is owner=<owner>.
	Selector string `json:"selector,omitempty"`
}

// ExecutorListRequest lists enrolled executors, optionally filtered.
type ExecutorListRequest struct {
	Selector string `json:"selector,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// ExecutorLabelRequest sets or removes labels on an executor's database row.
type ExecutorLabelRequest struct {
	ExecutorID string            `json:"executorId"`
	Set        map[string]string `json:"set,omitempty"`
	Remove     []string          `json:"remove,omitempty"`
}

// ExecutorDisableRequest disables an executor. Its credential stops working.
type ExecutorDisableRequest struct {
	ExecutorID string `json:"executorId"`
}

// ExecutorEnableRequest re-enables a disabled executor.
type ExecutorEnableRequest struct {
	ExecutorID string `json:"executorId"`
}

// ExecutorDeleteRequest permanently removes an executor row. Unlike disable,
// this cannot be undone.
type ExecutorDeleteRequest struct {
	ExecutorID string `json:"executorId"`
}

// ─── Identity ────────────────────────────────────────────────────────────────

// UserCreateResponseData carries the plaintext token. It is the only time it
// is ever transmitted: the daemon stores a digest and cannot reproduce it.
type UserCreateResponseData struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Token     string `json:"token"`
	CreatedAt string `json:"created_at,omitempty"`
}
