package store

import (
	"sync"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
)

// Session is the controller's per-child record. Pure metadata —
// I/O lives on the Child struct (internal/child).
//
// Holders must use Snapshot() to read; never expose *Session pointers
// outside this package.
type Session struct {
	mu sync.Mutex

	ChildID string
	PID     int
	Name    string
	Cwd     string

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

	// Counters
	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string

	// Handles into the live Child. Set by store.Insert from
	// Child setup; nil for sessions in "exited" state without an
	// associated Child.
	cmdCh chan<- []byte
	done  <-chan struct{}
}

// Snapshot is a defensive copy used at every boundary.
type Snapshot struct {
	ChildID string
	PID     int
	Name    string
	Cwd     string

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
		Provider: s.Provider, Model: s.Model, Thinking: s.Thinking,
		SessionID: s.SessionID, SessionFile: s.SessionFile,
		Status: s.Status, StartedAt: s.StartedAt, LastActivity: s.LastActivity,
		ExitedAt: s.ExitedAt, ExitCode: exitCode, ExitSignal: s.ExitSignal,

		NoSession: s.NoSession, SessionDir: s.SessionDir,
		ResumeSession: s.ResumeSession, ForkSession: s.ForkSession,
		Tools:          copyStrings(s.Tools),
		NoTools:        s.NoTools, NoBuiltinTools: s.NoBuiltinTools,
		Extensions:    copyStrings(s.Extensions),
		NoExtensions:  s.NoExtensions,
		Skills:        copyStrings(s.Skills),
		NoSkills:      s.NoSkills,
		PromptTemplates:   copyStrings(s.PromptTemplates),
		NoPromptTemplates: s.NoPromptTemplates,
		Themes:        copyStrings(s.Themes),
		NoThemes:      s.NoThemes,
		NoContextFiles: s.NoContextFiles,
		SystemPrompt: s.SystemPrompt, AppendSystemPrompt: s.AppendSystemPrompt,
		Verbose: s.Verbose, PiBinary: s.PiBinary,
		ExtraArgs: copyStrings(s.ExtraArgs),

		ExtensionErrors: s.ExtensionErrors,
		AutoRetries:     s.AutoRetries,
		LastRetryError:  s.LastRetryError,
		LastRetryFinal:  s.LastRetryFinal,
	}
}

func copyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
