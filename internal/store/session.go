package store

import (
	"sync"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/ring"
	"git.graveland.dev/brent/pi-controller/protocol"
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
	ForkSession        string
	Tools              []string
	NoTools            bool
	NoBuiltinTools     bool
	Extensions         []string
	NoExtensions       bool
	Skills             []string
	NoSkills           bool
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
	// Keys with the "pic/" prefix are reserved for auto-labels set by the daemon.
	Labels map[string]string

	// Handles into the live Child. Set by store.Insert from
	// Child setup; nil for sessions in "exited" state without an
	// associated Child.
	cmdCh chan<- []byte
	done  <-chan struct{}
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
		PromptTemplates:   copyStrings(s.PromptTemplates),
		NoPromptTemplates: s.NoPromptTemplates,
		Themes:            copyStrings(s.Themes),
		NoThemes:          s.NoThemes,
		NoContextFiles:    s.NoContextFiles,
		SystemPrompt:      s.SystemPrompt, AppendSystemPrompt: s.AppendSystemPrompt,
		Verbose: s.Verbose, PiBinary: s.PiBinary,
		ExtraArgs: copyStrings(s.ExtraArgs),

		ExtensionErrors: s.ExtensionErrors,
		AutoRetries:     s.AutoRetries,
		LastRetryError:  s.LastRetryError,
		LastRetryFinal:  s.LastRetryFinal,
		ExitedRing:       copyRingEvents(s.ExitedRing),
		ExitedRenderRing: copyRingEvents(s.ExitedRenderRing),
		Labels:           copyLabels(s.Labels),
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
