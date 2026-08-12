// Package protocol defines the typed wire shapes for every ctrl_* command,
// response, and event in the pi-controller protocol. This is a pure-data
// package: no logic, no I/O. Field names and JSON tags match the spec exactly.
//
// Cross-references:
//
//	§6  — client → controller commands (requests)
//	§7  — controller → client events
//	§8  — error codes
//	§10 — status constants
package protocol

import "encoding/json"

// ─── Type constants ──────────────────────────────────────────────────────────

const (
	TypeCtrlDaemonShutdown     = "ctrl_daemon_shutdown"
	TypeCtrlList               = "ctrl_list"
	TypeCtrlGet                = "ctrl_get"
	TypeCtrlListModels         = "ctrl_list_models"
	TypeCtrlListPresets        = "ctrl_list_presets"
	TypeCtrlSpawn              = "ctrl_spawn"
	TypeCtrlResume             = "ctrl_resume"
	TypeCtrlKill               = "ctrl_kill"
	TypeCtrlAuth               = "ctrl_auth"
	TypeCtrlSubscribe          = "ctrl_subscribe"
	TypeCtrlUnsubscribe        = "ctrl_unsubscribe"
	TypeCtrlGlobalSubscribe    = "ctrl_global_subscribe"
	TypeCtrlGlobalUnsubscribe  = "ctrl_global_unsubscribe"
	TypeCtrlGetRecent          = "ctrl_get_recent"
	TypeCtrlGetStreams         = "ctrl_get_streams"
	TypeCtrlSend               = "ctrl_send"
	TypeCtrlForget             = "ctrl_forget"
	TypeCtrlForgetAllExited    = "ctrl_forget_all_exited"
	TypeCtrlSearch             = "ctrl_search"
	TypeCtrlStatus             = "ctrl_status"
	TypeCtrlSetLabels          = "ctrl_set_labels"
	TypeCtrlResponse           = "ctrl_response"
	TypeCtrlEvent              = "ctrl_event"
	TypeCtrlChildSpawned       = "ctrl_child_spawned"
	TypeCtrlChildExited        = "ctrl_child_exited"
	TypeCtrlChildStatus        = "ctrl_child_status"
	TypeCtrlChildRenamed       = "ctrl_child_renamed"
	TypeCtrlChildLabeled       = "ctrl_child_labeled"
	TypeCtrlConversationStats  = "ctrl_conversation_stats"
	TypeCtrlConversationSearch = "ctrl_conversation_search"
	TypeCtrlConversationExport = "ctrl_conversation_export"
)

// ─── Status constants (§10) ──────────────────────────────────────────────────

// Status is the state of a pi child process.
type Status string

const (
	StatusSpawning     Status = "spawning"
	StatusIdle         Status = "idle"
	StatusStreaming    Status = "streaming"
	StatusToolRunning  Status = "tool_running"
	StatusCompacting   Status = "compacting"
	StatusBlockedUI    Status = "blocked_ui"
	StatusShuttingDown Status = "shutting_down"
	StatusExited       Status = "exited"
)

// ─── Error code constants (§8) ───────────────────────────────────────────────

const (
	// ErrChildNotFound is returned when no child with the given childId exists.
	ErrChildNotFound = "child_not_found"
	// ErrChildExited is returned when the child has already exited.
	ErrChildExited = "child_exited"
	// ErrChildInGrace is equivalent to ErrChildExited; explicit for clarity.
	ErrChildInGrace = "child_in_grace"
	// ErrChildShuttingDown is returned when stdin is closed during graceful shutdown.
	ErrChildShuttingDown = "child_shutting_down"
	// ErrNotResumable is returned by ctrl_resume when the child is not in exited status.
	ErrNotResumable = "not_resumable"
	// ErrNotExited is returned by ctrl_forget when the child is still live.
	ErrNotExited = "not_exited"
	// ErrSessionFileMissing is returned by ctrl_resume when the session file is gone.
	ErrSessionFileMissing = "session_file_missing"
	// ErrBackpressure is returned when the child's command channel is full.
	ErrBackpressure = "backpressure"
	// ErrInvalidArgs is returned when request fields fail validation.
	ErrInvalidArgs = "invalid_args"
	// ErrSpawnFailed is returned when the pi subprocess fails to start.
	ErrSpawnFailed = "spawn_failed"
	// ErrAuthRequired is returned on TCP connections that skip ctrl_auth.
	ErrAuthRequired = "auth_required"
	// ErrAuthInvalid is returned when the TCP auth token does not match.
	ErrAuthInvalid = "auth_invalid"
	// ErrNotFound is the generic not-found error (e.g., ctrl_resume against unknown id).
	ErrNotFound = "not_found"
	// ErrInternal is returned on unexpected controller-side errors.
	ErrInternal = "internal"
	// ErrNoAgentDB is returned by ctrl_conversation_* commands when the
	// daemon has no agent database configured (RAFIKI_DB unset).
	ErrNoAgentDB = "no_agent_db"
	// ErrPayloadTooLarge is returned by ctrl_conversation_export when the
	// marshaled transcript would exceed the maximum response frame size.
	ErrPayloadTooLarge = "payload_too_large"
)

// ─── Shared sub-shapes ───────────────────────────────────────────────────────

// ListFilter narrows ctrl_list results. All fields are optional (§6.1).
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

// SubscribeFilter selects which pi events are forwarded on a subscription (§6.7, §6.8).
// Filter resolution: (profile members) ∪ include − exclude.
type SubscribeFilter struct {
	Profile string   `json:"profile,omitempty"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// SearchSessionFilter narrows which children ctrl_search scans (§6.15).
// Labels/HasLabel apply the same AND-match semantics as ListFilter.
type SearchSessionFilter struct {
	CwdContains  string            `json:"cwdContains,omitempty"`
	NameContains string            `json:"nameContains,omitempty"`
	Since        int64             `json:"since,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	HasLabel     []string          `json:"hasLabel,omitempty"`
}

// ─── Request types (§6) ──────────────────────────────────────────────────────

// ListRequest lists children known to the controller (§6.1).
type ListRequest struct {
	Type   string      `json:"type"`
	ID     string      `json:"id,omitempty"`
	Filter *ListFilter `json:"filter,omitempty"`
}

// GetRequest retrieves a snapshot of one child by id (§6.2).
type GetRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
}

// SpawnRequest starts a new pi child (§6.3).
// cwd is required; all other fields are optional and forwarded to pi as flags.
// apiKey is used at spawn time only and is never written to the state record.
type SpawnRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`

	// Kind selects the child protocol/binary: "pi" (default, when empty) or
	// "claude" (Claude Code CLI, driven over stream-json).
	Kind string `json:"kind,omitempty"`

	// ConfigDir, for kind=claude, is exported to the child as CLAUDE_CONFIG_DIR
	// — it selects the claude config dir (plugins, hooks, MCP, settings). It is
	// persisted so a resumed claude child re-uses the same profile.
	ConfigDir string `json:"configDir,omitempty"`

	// Identity
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"` // user-supplied labels; rafiki/ prefix rejected

	// Working directory (required, absolute).
	Cwd string `json:"cwd"`

	// Model + auth (pi resolves from its own config when omitted).
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"` // off|minimal|low|medium|high|xhigh
	APIKey   string `json:"apiKey,omitempty"`

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
	MCPConfig         string   `json:"mcpConfig,omitempty"`
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

	// ResumedFromSession is set by `rafiki resume --pi-session` when spawning a
	// fresh child to continue a pi session.jsonl that was not previously
	// managed by rafiki.  When non-empty the daemon adds the reserved auto-label
	// `rafiki/resumed-from-session=<value>` to the new child.  Sent here (rather
	// than in Labels) because the `rafiki/` namespace is reserved for daemon
	// auto-labels; user-supplied Labels with that prefix are rejected.
	ResumedFromSession string `json:"resumedFromSession,omitempty"`

	// RecordRequests, when true, records raw LLM API request/response pairs
	// to the debug raw_http_request hypertable (agent-kind only; requires
	// RAFIKI_RECORD_REQUESTS=1 at daemon startup).
	RecordRequests bool `json:"recordRequests,omitempty"`

	// ExecutorSelector is a label selector that narrows the parent's executor
	// set for this child. Used with the executor pool: the child runs its
	// filesystem and shell tools on an executor that matches both its
	// parent's set (intersected) and this selector.
	ExecutorSelector string `json:"executorSelector,omitempty"`

	// WorkspaceMode controls how the executor provisions this child's
	// workspace: "ephemeral" for one-shot, "pinned" for durable.
	WorkspaceMode string `json:"workspaceMode,omitempty"`

	// ParentChildID is the ChildID of the parent agent. Set when spawning a
	// sub-agent so the daemon can compute the effective executor set by
	// narrowing from the parent rather than from the full pool.
	ParentChildID string `json:"parentChildId,omitempty"`
}

// ResumeRequest re-spawns a child against its persisted state record (§6.4).
// The child must be in exited status. apiKey is optional and not persisted.
type ResumeRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
	APIKey  string `json:"apiKey,omitempty"`
}

// KillRequest stops a running child gracefully, escalating to SIGKILL if needed (§6.5).
type KillRequest struct {
	Type              string `json:"type"`
	ID                string `json:"id,omitempty"`
	ChildID           string `json:"childId"`
	ShutdownTimeoutMs int64  `json:"shutdownTimeoutMs,omitempty"`
	KillTimeoutMs     int64  `json:"killTimeoutMs,omitempty"`
}

// AuthRequest authenticates a TCP connection (§6.6).
// Must be the first frame on a TCP connection; UDS connections skip this.
type AuthRequest struct {
	Type  string `json:"type"`
	ID    string `json:"id,omitempty"`
	Token string `json:"token"`
}

// SubscribeRequest subscribes to events from one child or a label-filtered
// set of children (§6.7).  Default profile when filter is omitted: firehose.
//
// Mode resolution:
//   - ChildID set, Labels/HasLabel empty → per-child subscription.
//   - ChildID empty, Labels/HasLabel set → label-filtered subscription:
//     events from every child currently matching the filter, including
//     children that spawn or are relabelled into a match later.
//   - ChildID set AND Labels/HasLabel set → error (mutually exclusive).
type SubscribeRequest struct {
	Type    string           `json:"type"`
	ID      string           `json:"id,omitempty"`
	ChildID string           `json:"childId,omitempty"`
	Filter  *SubscribeFilter `json:"filter,omitempty"`
	// Labels and HasLabel select the label-filtered mode.  AND-match across
	// Labels entries; HasLabel tests key presence only.
	Labels   map[string]string `json:"labels,omitempty"`
	HasLabel []string          `json:"hasLabel,omitempty"`
}

// UnsubscribeRequest removes the per-child subscription for this connection (§6.9).
type UnsubscribeRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
}

// GlobalSubscribeRequest subscribes to controller-wide lifecycle events (§6.10).
// Global subscribers see only ctrl_child_* events, never per-child ctrl_event frames.
type GlobalSubscribeRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// GlobalUnsubscribeRequest cancels a global subscription (§6.10).
type GlobalUnsubscribeRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// GetRecentRequest queries the per-child replay buffer (§6.11).
type GetRecentRequest struct {
	Type    string   `json:"type"`
	ID      string   `json:"id,omitempty"`
	ChildID string   `json:"childId"`
	Limit   int      `json:"limit,omitempty"`
	Since   int64    `json:"since,omitempty"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`

	Rendered bool `json:"rendered,omitempty"`
}

// SendRequest forwards a pi-RPC frame to a child's stdin (§6.12).
// The frame field is forwarded verbatim; the controller does not inspect it.
type SendRequest struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	ChildID string          `json:"childId"`
	Frame   json.RawMessage `json:"frame"`
}

// ForgetRequest drops an exited child from in-memory state (§6.13).
// Only valid when the child is in exited status.
type ForgetRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
}

// ForgetAllExitedRequest removes all exited children from in-memory state (§6.14).
// OlderThanMs filters by age; zero means all exited entries.
type ForgetAllExitedRequest struct {
	Type        string `json:"type"`
	ID          string `json:"id,omitempty"`
	OlderThanMs int64  `json:"olderThanMs,omitempty"`
}

// SearchRequest searches in-memory content across children (§6.15).
type SearchRequest struct {
	Type          string               `json:"type"`
	ID            string               `json:"id,omitempty"`
	Query         string               `json:"query"`
	Regex         bool                 `json:"regex,omitempty"`
	Limit         int                  `json:"limit,omitempty"`
	Context       int                  `json:"context,omitempty"`
	SessionFilter *SearchSessionFilter `json:"sessionFilter,omitempty"`
}

// StatusRequest queries daemon health and statistics (§6.16).
type StatusRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// ConversationStatsRequest queries persisted conversation stats: global
// (filtered) when ConversationID is empty, scoped to one conversation
// otherwise — in which case the filter fields below are ignored (§6.17).
// SinceUnix/UntilUnix are Unix seconds; 0 means unbounded.
type ConversationStatsRequest struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	SinceUnix      int64  `json:"sinceUnix,omitempty"`
	UntilUnix      int64  `json:"untilUnix,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Persona        string `json:"persona,omitempty"`
	Source         string `json:"source,omitempty"`
	Model          string `json:"model,omitempty"`
	Path           string `json:"path,omitempty"`
}

// ConversationSearchRequest searches persisted conversation history (§6.18).
// SinceUnix/UntilUnix are Unix seconds; 0 means unbounded.
type ConversationSearchRequest struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	SinceUnix int64  `json:"sinceUnix,omitempty"`
	UntilUnix int64  `json:"untilUnix,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Persona   string `json:"persona,omitempty"`
	Source    string `json:"source,omitempty"`
	Model     string `json:"model,omitempty"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status,omitempty"`
	MinTokens int64  `json:"minTokens,omitempty"`
	Text      string `json:"text,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// ConversationExportRequest fetches one conversation's full transcript (§6.19).
type ConversationExportRequest struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	ConversationID string `json:"conversationId"`
}

// ─── Response envelope and per-command response data types ───────────────────

// Response is the generic ctrl_response envelope. The Data field is left as
// json.RawMessage so consumers can decode into the per-command type lazily.
type Response struct {
	Type    string          `json:"type"`
	Command string          `json:"command"`
	ID      string          `json:"id,omitempty"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *ErrorBody      `json:"error,omitempty"`
}

// ErrorBody carries the machine-readable code and human-readable message (§8).
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// ChildSummary is a single entry in ctrl_list / ctrl_get response data (§6.1).
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
}

// ListResponseData is the data payload for ctrl_list responses.
type ListResponseData struct {
	Children []ChildSummary `json:"children"`
}

// GetResponseData is the data payload for ctrl_get responses (§6.2).
// The shape is identical to a single ChildSummary entry in ctrl_list.
type GetResponseData = ChildSummary

// SpawnResponseData is the data payload for ctrl_spawn and ctrl_resume responses (§6.3).
// When Stalled is true, the child started but did not respond to the initial get_state;
// the other fields will be empty in that case.
type SpawnResponseData struct {
	ChildID     string `json:"childId"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionFile string `json:"sessionFile,omitempty"`
	Model       string `json:"model,omitempty"`
	Stalled     bool   `json:"stalled"`
}

// KillResponseData is the data payload for ctrl_kill responses (§6.5).
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

// GetRecentResponseData is the data payload for ctrl_get_recent responses (§6.11).
// Each element of Events is a verbatim pi event in publish order.
type GetRecentResponseData struct {
	Events           []json.RawMessage `json:"events"`
	TotalInBuffer    int               `json:"totalInBuffer"`
	OldestTimestamp  int64             `json:"oldestTimestamp"`
	TruncatedByLimit bool              `json:"truncatedByLimit"`
	// TruncatedBySize reports that oldest events were dropped so the response
	// frame stays under the MaxFrameBytes reader cap.
	TruncatedBySize bool `json:"truncatedBySize,omitempty"`
}

// GetStreamsRequest queries a live child's in-memory stdin/stderr capture.
// Which selects the streams: "in", "err", or "all" ("" means "all").
//
// Note: live stderr is never served. The child's stderr buffer is an unguarded
// in-memory buffer written by a reader goroutine, so snapshotting it while the
// child runs would race. "err" and "all" therefore only ever return stdin for a
// live child; stderr is available exclusively post-exit via the on-disk dump,
// which the CLI falls back to.
type GetStreamsRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
	Which   string `json:"which,omitempty"` // "in" | "err" | "all"; default "all"
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

// ForgetAllExitedResponseData is the data payload for ctrl_forget_all_exited responses.
type ForgetAllExitedResponseData struct {
	Count int `json:"count"`
}

// SearchHit is one content match in a ctrl_search response (§6.15).
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

// SearchResponseData is the data payload for ctrl_search responses (§6.15).
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

// StatusResponseData is the data payload for ctrl_status responses (§6.16).
type StatusResponseData struct {
	Version     string      `json:"version"`
	StartedAt   int64       `json:"startedAt"`
	Children    ChildCounts `json:"children"`
	MemoryBytes int64       `json:"memoryBytes"`
	Socket      string      `json:"socket,omitempty"`
	LogsDir     string      `json:"logsDir,omitempty"`
}

// ─── Daemon-level events ────────────────────────────────────────────────────

// CtrlDaemonShutdown is broadcast to all active connections when the daemon
// begins its own shutdown sequence (SIGTERM/SIGINT/SIGHUP).  Children are
// still being gracefully shut down at this point — this is purely advance
// warning so clients can exit cleanly rather than hanging on broken pipes.
type CtrlDaemonShutdown struct {
	Type   string `json:"type"`             // "ctrl_daemon_shutdown"
	Reason string `json:"reason,omitempty"` // e.g. "signal received: terminated"
}

// ─── Event types (§7) ────────────────────────────────────────────────────────

// CtrlEvent wraps a pi-RPC event from a subscribed child (§7.1).
// The Event field is forwarded verbatim; the controller never modifies it.
type CtrlEvent struct {
	Type    string          `json:"type"`
	ChildID string          `json:"childId"`
	Event   json.RawMessage `json:"event"`
}

// CtrlChildSpawned is emitted when a child process starts (§7.2).
// Delivered to global subscribers and per-child subscribers of this child.
type CtrlChildSpawned struct {
	Type    string `json:"type"`
	ChildID string `json:"childId"`
	Name    string `json:"name,omitempty"`
	Cwd     string `json:"cwd"`
	PID     int    `json:"pid"`
	Model   string `json:"model,omitempty"`
	At      int64  `json:"at"`
}

// CtrlChildExited is emitted when a child process exits (§7.3).
// ExitCode is nil when the child was killed by signal.
// Signal is absent (not "null") when the child exited normally.
type CtrlChildExited struct {
	Type       string  `json:"type"`
	ChildID    string  `json:"childId"`
	ExitCode   *int    `json:"exitCode"`
	Signal     string  `json:"signal,omitempty"`
	LastStatus string  `json:"lastStatus"`
	Duration   float64 `json:"duration"` // seconds
	At         int64   `json:"at"`
}

// CtrlChildStatus is emitted on every state transition (§7.4).
type CtrlChildStatus struct {
	Type     string `json:"type"`
	ChildID  string `json:"childId"`
	Status   string `json:"status"`
	Previous string `json:"previous"`
	At       int64  `json:"at"`
}

// CtrlChildRenamed is emitted when a child's name changes (§7.5).
type CtrlChildRenamed struct {
	Type     string `json:"type"`
	ChildID  string `json:"childId"`
	Name     string `json:"name"`
	Previous string `json:"previous"`
	At       int64  `json:"at"`
}

// SetLabelsRequest mutates the labels on an existing child (§6.17).
// Set entries are applied first, then Remove entries are deleted.
// Keys using the rafiki/ prefix are reserved and rejected with ErrInvalidArgs.
type SetLabelsRequest struct {
	Type    string            `json:"type"` // "ctrl_set_labels"
	ID      string            `json:"id,omitempty"`
	ChildID string            `json:"childId"`
	Set     map[string]string `json:"set,omitempty"`
	Remove  []string          `json:"remove,omitempty"`
}

// SetLabelsResponseData is the data payload for ctrl_set_labels responses.
// Labels carries the full post-mutation map.
type SetLabelsResponseData struct {
	Labels map[string]string `json:"labels"`
}

// CtrlChildLabeled is broadcast to subscribers when labels change (§7.6).
// Labels contains the full post-mutation map, not a delta.
type CtrlChildLabeled struct {
	Type    string            `json:"type"` // "ctrl_child_labeled"
	ChildID string            `json:"childId"`
	Labels  map[string]string `json:"labels"` // complete post-mutation label set
}

// ─── ctrl_list_models ────────────────────────────────────────────────────────

// ListModelsRequest enumerates LLM models from all configured sources.
// Provider is an optional filter; when non-empty only models whose provider
// field matches are returned.
type ListModelsRequest struct {
	Type     string `json:"type"` // "ctrl_list_models"
	ID       string `json:"id,omitempty"`
	Provider string `json:"provider,omitempty"` // optional provider filter
}

// ListModelsResponseData is the data payload for ctrl_list_models responses.
type ListModelsResponseData struct {
	Models []ModelInfo `json:"models"`
}

// ModelInfo is one entry in a ctrl_list_models response.
type ModelInfo struct {
	ID       string `json:"id"` // "provider/model"
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Name     string `json:"name,omitempty"` // display name from models.json
	Source   string `json:"source"`         // user-config | builtin | ollama | lmstudio
}

// ─── ctrl_list_presets ───────────────────────────────────────────────────────

// ListPresetsRequest enumerates presets from <config dir>/presets.json.
// Labels and HasLabel filter results with the same AND-match semantics as
// ctrl_list: all Labels k=v pairs must match and all HasLabel keys must be
// present on the preset's labels map.
type ListPresetsRequest struct {
	Type     string            `json:"type"` // "ctrl_list_presets"
	ID       string            `json:"id,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`   // AND-match: all k=v must match
	HasLabel []string          `json:"hasLabel,omitempty"` // key presence only
}

// ListPresetsResponseData is the data payload for ctrl_list_presets responses.
type ListPresetsResponseData struct {
	Presets []PresetInfo `json:"presets"`
}

// PresetInfo is one entry in a ctrl_list_presets response.
type PresetInfo struct {
	Name   string            `json:"name"`
	Model  string            `json:"model,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}
