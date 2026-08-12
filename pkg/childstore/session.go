package childstore

import (
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
)

// Session is the controller's per-child record. Pure metadata —
// I/O lives on the Child struct (internal/child).
//
// Holders must use Snapshot() to read; never expose *Session pointers
// outside this package.
type Session struct {
	// mu guards every exported field below. All field reads should go through
	// Snapshot() (which takes the lock). Writes within the store package must
	// also hold mu.
	mu sync.Mutex

	ChildID string
	PID     int
	Name    string
	Cwd     string
	Kind    string // "pi" (default) or "claude"; selects the child protocol

	// ConfigDir, for claude children, is exported as CLAUDE_CONFIG_DIR.
	ConfigDir string

	Provider string
	Model    string
	Thinking string

	SessionID   string
	SessionFile string

	Status       protocol.Status
	StartedAt    time.Time
	LastActivity time.Time
	ExitedAt     time.Time
	ExitCode     *int
	ExitSignal   string

	// Spawn configuration (subset persisted to state record).

	// NoSession means the child runs in ephemeral mode with no session.jsonl
	// (pi's --no-session flag). Not the same as having no SessionID.
	NoSession bool

	SessionDir string

	// ResumeSession is an absolute path to an existing session.jsonl that the
	// child should open via pi's --session flag.
	ResumeSession string

	// ForkSession is an absolute path to an existing session.jsonl that the
	// child should fork from via pi's --fork flag.
	ForkSession    string
	Tools          []string
	NoTools        bool
	NoBuiltinTools bool
	Extensions     []string
	NoExtensions   bool
	Skills         []string
	NoSkills       bool
	// SkillsDirs are extra skill directories for an agent-kind child, so a
	// resumed child rejoins with the same skill inventory it was spawned with.
	SkillsDirs []string
	// MCPConfig is the .mcp.json path for an agent-kind child, carried across
	// resume for the same reason.
	MCPConfig          string
	PromptTemplates    []string
	NoPromptTemplates  bool
	Themes             []string
	NoThemes           bool
	NoContextFiles     bool
	SystemPrompt       string
	AppendSystemPrompt string
	Verbose            bool
	PiBinary           string
	ExtraArgs          []string

	// RecordRequests, when true, enables raw LLM API request/response capture
	// for this child (see protocol.SpawnRequest.RecordRequests). Persisted and
	// carried across resume so a child started with --record-requests keeps
	// capturing after the daemon restarts or the session is respawned.
	RecordRequests bool

	// ExecutorSelector is a label selector narrowing the parent's executor set
	// for this child. Persisted so a resumed child retains the same confinement.
	ExecutorSelector string

	// WorkspaceMode controls the executor's workspace provisioning.
	// Persisted for the same reason as ExecutorSelector.
	WorkspaceMode string

	// Counters
	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string

	// ExitedRing holds a snapshot of the ring buffer captured when the child
	// exited. It is populated by handleChildExit before the live Child is
	// removed, so ctrl_get_recent remains queryable after exit (spec §11.4).
	ExitedRing []ring.Event

	// ExitedRenderRing snapshots the render-ring at exit (normalizing children
	// only), so the rendered ctrl_get_recent view survives the child's removal.
	ExitedRenderRing []ring.Event

	// Labels holds arbitrary user-defined and auto-derived key=value metadata.
	// Keys with the "rafiki/" prefix are reserved for auto-labels set by the daemon.
	//
	// The prefix was "fundi/" before the rename to rafiki. This is a persisted
	// value, not just a code identifier: a child spawned (or resumed) before
	// the rename keeps "fundi/kind"/"fundi/model"/etc. on its record forever —
	// there is no migration that rewrites old rows. A `--has-label rafiki/kind=…`
	// filter written after upgrading will silently NOT match a pre-rename
	// child's still-"fundi/kind"-labeled record. See docs/MIGRATING.md.
	Labels map[string]string

	// SlashCommands is the claude child's advertised slash-command list (names),
	// captured from its init frame. Empty for pi children.
	SlashCommands []string
}

// Snapshot is a defensive copy used at every boundary.
type Snapshot struct {
	ChildID   string
	PID       int
	Name      string
	Cwd       string
	Kind      string
	ConfigDir string

	Provider string
	Model    string
	Thinking string

	SessionID   string
	SessionFile string

	Status       protocol.Status
	StartedAt    time.Time
	LastActivity time.Time
	ExitedAt     time.Time
	ExitCode     *int
	ExitSignal   string

	NoSession          bool
	SessionDir         string
	ResumeSession      string
	ForkSession        string
	Tools              []string
	NoTools            bool
	NoBuiltinTools     bool
	Extensions         []string
	NoExtensions       bool
	Skills             []string
	NoSkills           bool
	SkillsDirs         []string
	MCPConfig          string
	PromptTemplates    []string
	NoPromptTemplates  bool
	Themes             []string
	NoThemes           bool
	NoContextFiles     bool
	SystemPrompt       string
	AppendSystemPrompt string
	Verbose            bool
	PiBinary           string
	ExtraArgs          []string

	RecordRequests bool

	// ExecutorSelector narrows the parent's executor set.
	ExecutorSelector string
	// WorkspaceMode controls executor workspace provisioning.
	WorkspaceMode string

	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string

	// ExitedRing is a snapshot of the ring buffer taken at exit time.
	// Populated for exited sessions; nil for live sessions.
	ExitedRing []ring.Event

	// ExitedRenderRing is a snapshot of the render-ring taken at exit time.
	// Populated for exited normalizing sessions; nil otherwise.
	ExitedRenderRing []ring.Event

	// Labels holds arbitrary user-defined and auto-derived key=value metadata.
	// This is a defensive copy; callers may mutate it freely.
	Labels map[string]string

	// SlashCommands is the claude child's advertised slash-command list (names),
	// captured from its init frame. Empty for pi children.
	SlashCommands []string
}

// Snapshot returns a deep copy of the session's fields. The caller may freely
// mutate the returned value; it shares no backing storage with the Session.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	exitCode := s.ExitCode
	if exitCode != nil {
		v := *exitCode
		exitCode = &v
	}
	return Snapshot{
		ChildID: s.ChildID, Name: s.Name, Cwd: s.Cwd, PID: s.PID,
		Kind: s.Kind, ConfigDir: s.ConfigDir,
		Provider: s.Provider, Model: s.Model, Thinking: s.Thinking,
		SessionID: s.SessionID, SessionFile: s.SessionFile,
		Status: s.Status, StartedAt: s.StartedAt, LastActivity: s.LastActivity,
		ExitedAt: s.ExitedAt, ExitCode: exitCode, ExitSignal: s.ExitSignal,

		NoSession: s.NoSession, SessionDir: s.SessionDir,
		ResumeSession: s.ResumeSession, ForkSession: s.ForkSession,
		Tools:   copyStrings(s.Tools),
		NoTools: s.NoTools, NoBuiltinTools: s.NoBuiltinTools,
		Extensions:        copyStrings(s.Extensions),
		NoExtensions:      s.NoExtensions,
		Skills:            copyStrings(s.Skills),
		NoSkills:          s.NoSkills,
		SkillsDirs:        copyStrings(s.SkillsDirs),
		MCPConfig:         s.MCPConfig,
		PromptTemplates:   copyStrings(s.PromptTemplates),
		NoPromptTemplates: s.NoPromptTemplates,
		Themes:            copyStrings(s.Themes),
		NoThemes:          s.NoThemes,
		NoContextFiles:    s.NoContextFiles,
		SystemPrompt:      s.SystemPrompt, AppendSystemPrompt: s.AppendSystemPrompt,
		Verbose: s.Verbose, PiBinary: s.PiBinary,
		ExtraArgs: copyStrings(s.ExtraArgs),

		RecordRequests: s.RecordRequests,

		ExecutorSelector: s.ExecutorSelector,
		WorkspaceMode:    s.WorkspaceMode,
		ExtensionErrors:  s.ExtensionErrors,
		AutoRetries:      s.AutoRetries,
		LastRetryError:   s.LastRetryError,
		LastRetryFinal:   s.LastRetryFinal,
		ExitedRing:       copyRingEvents(s.ExitedRing),
		ExitedRenderRing: copyRingEvents(s.ExitedRenderRing),
		Labels:           copyLabels(s.Labels),
		SlashCommands:    copyStrings(s.SlashCommands),
	}
}

// copyRingEvents returns a defensive copy of a ring event slice.
func copyRingEvents(events []ring.Event) []ring.Event {
	if len(events) == 0 {
		return nil
	}
	out := make([]ring.Event, len(events))
	for i, e := range events {
		cp := make([]byte, len(e.Bytes))
		copy(cp, e.Bytes)
		out[i] = ring.Event{Bytes: cp, Timestamp: e.Timestamp}
	}
	return out
}

// copyLabels returns a defensive copy of a label map. Both nil and empty
// inputs return nil.
func copyLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// copyStrings returns a fresh slice with a copy of s's elements.
// Both nil and empty inputs return nil — callers that need to distinguish
// "set to empty" from "absent" should handle that at the field level.
func copyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
