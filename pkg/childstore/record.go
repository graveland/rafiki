// SPDX-License-Identifier: Apache-2.0

package childstore

import (
	"context"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ChildRecord is one child's durable state: the flat columns of
// conversations.child plus the config blob.
//
// The split between flat columns and Config is not aesthetic. A field is flat
// when something filters, sorts or joins on it; everything else is read exactly
// once, at resume, as a unit, and belongs in Config where adding a field costs
// no migration.
type ChildRecord struct {
	ChildID        string
	ConversationID string // empty means unknown; stored as SQL NULL
	OwnerUserID    string // empty means unattributed; stored as SQL NULL

	Kind      string
	Name      string
	Cwd       string
	ConfigDir string
	PID       int
	DaemonID  string
	NSToken   string

	Provider string
	Model    string
	Thinking string

	SessionFile string
	SessionDir  string
	SessionID   string
	NoSession   bool

	// Status is the child's current state. LastStatus is the state it was in
	// before it exited, written only by the exit path, and it is what the
	// recovery predicate reads.
	Status       string
	LastStatus   string
	SpawnedAt    time.Time
	LastActivity time.Time
	ExitedAt     time.Time
	ExitCode     *int
	ExitSignal   string

	// UpdatedAt is when this row was last written, maintained by the database
	// itself (DEFAULT now() on insert, updated_at = now() on every upsert). It
	// is the one liveness signal a reader can trust without trusting the
	// writer: a fresh row proves its daemon was alive when it wrote, which is
	// what recovery's adoption gate keys on (see cmd/rafikid's
	// foreignFreshGrace) — a lease can be absent because it was never yet
	// acquired, but a row cannot be fresh unless its writer was alive.
	UpdatedAt time.Time

	ExecutorSelector string
	WorkspaceMode    string

	MaxDepth    int
	MaxCost     float64
	MaxChildren int

	Config ChildConfig
	Labels map[string]string
}

// ChildConfig is the spawn configuration that only matters on resume. It is
// stored as the config JSONB column.
type ChildConfig struct {
	ResumeSession      string   `json:"resumeSession,omitempty"`
	ForkSession        string   `json:"forkSession,omitempty"`
	Tools              []string `json:"tools,omitempty"`
	NoTools            bool     `json:"noTools,omitempty"`
	NoBuiltinTools     bool     `json:"noBuiltinTools,omitempty"`
	Extensions         []string `json:"extensions,omitempty"`
	NoExtensions       bool     `json:"noExtensions,omitempty"`
	Skills             []string `json:"skills,omitempty"`
	NoSkills           bool     `json:"noSkills,omitempty"`
	SkillsDirs         []string `json:"skillsDirs,omitempty"`
	MCPConfig          string   `json:"mcpConfig,omitempty"`
	MCPServers         []string `json:"mcpServers,omitempty"`
	NoMCP              bool     `json:"noMcp,omitempty"`
	PromptTemplates    []string `json:"promptTemplates,omitempty"`
	NoPromptTemplates  bool     `json:"noPromptTemplates,omitempty"`
	Themes             []string `json:"themes,omitempty"`
	NoThemes           bool     `json:"noThemes,omitempty"`
	NoContextFiles     bool     `json:"noContextFiles,omitempty"`
	SystemPrompt       string   `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	Verbose            bool     `json:"verbose,omitempty"`
	PiBinary           string   `json:"piBinary,omitempty"`
	ExtraArgs          []string `json:"extraArgs,omitempty"`
	RecordRequests     bool     `json:"recordRequests,omitempty"`
}

// ChildStore persists child state records.
//
// One implementation exists (Postgres). The interface is a seam that keeps SQL
// out of the controller and makes the store testable without a live database,
// not an invitation to write a second backend.
type ChildStore interface {
	Upsert(ctx context.Context, rec ChildRecord) error
	Delete(ctx context.Context, childID string) error
	List(ctx context.Context) ([]ChildRecord, error)
}

// RecordFromSnapshot builds a durable record from a store snapshot.
//
// LastStatus is deliberately left empty: only the exit path knows a child's
// pre-exit state, and the upsert in pkg/childstoredb COALESCEs an empty value
// so an ordinary status write cannot blank the column.
func RecordFromSnapshot(snap Snapshot) ChildRecord {
	return ChildRecord{
		ChildID:   snap.ChildID,
		Kind:      snap.Kind,
		Name:      snap.Name,
		Cwd:       snap.Cwd,
		ConfigDir: snap.ConfigDir,
		PID:       snap.PID,

		Provider: snap.Provider,
		Model:    snap.Model,
		Thinking: snap.Thinking,

		SessionFile: snap.SessionFile,
		SessionDir:  snap.SessionDir,
		SessionID:   snap.SessionID,
		NoSession:   snap.NoSession,

		Status:       string(snap.Status),
		SpawnedAt:    snap.StartedAt,
		LastActivity: snap.LastActivity,
		ExitedAt:     snap.ExitedAt,
		ExitCode:     snap.ExitCode,
		ExitSignal:   snap.ExitSignal,

		ExecutorSelector: snap.ExecutorSelector,
		WorkspaceMode:    snap.WorkspaceMode,

		MaxDepth:    snap.MaxDepth,
		MaxCost:     snap.MaxCost,
		MaxChildren: snap.MaxChildren,

		Config: ChildConfig{
			ResumeSession:      snap.ResumeSession,
			ForkSession:        snap.ForkSession,
			Tools:              snap.Tools,
			NoTools:            snap.NoTools,
			NoBuiltinTools:     snap.NoBuiltinTools,
			Extensions:         snap.Extensions,
			NoExtensions:       snap.NoExtensions,
			Skills:             snap.Skills,
			NoSkills:           snap.NoSkills,
			SkillsDirs:         snap.SkillsDirs,
			MCPConfig:          snap.MCPConfig,
			MCPServers:         snap.MCPServers,
			NoMCP:              snap.NoMCP,
			PromptTemplates:    snap.PromptTemplates,
			NoPromptTemplates:  snap.NoPromptTemplates,
			Themes:             snap.Themes,
			NoThemes:           snap.NoThemes,
			NoContextFiles:     snap.NoContextFiles,
			SystemPrompt:       snap.SystemPrompt,
			AppendSystemPrompt: snap.AppendSystemPrompt,
			Verbose:            snap.Verbose,
			PiBinary:           snap.PiBinary,
			ExtraArgs:          snap.ExtraArgs,
			RecordRequests:     snap.RecordRequests,
		},
		Labels: snap.Labels,
	}
}

// SessionFromRecord rebuilds a Session from a durable record.
//
// ExitedRing and ExitedRenderRing are NOT restored — they are not persisted
// (they can be megabytes and they die with the daemon today), so a recovered
// session has nil rings, which ctrl_get_recent already handles.
func SessionFromRecord(rec ChildRecord) *Session {
	return &Session{
		ChildID:   rec.ChildID,
		PID:       rec.PID,
		Name:      rec.Name,
		Cwd:       rec.Cwd,
		Kind:      rec.Kind,
		ConfigDir: rec.ConfigDir,

		Provider: rec.Provider,
		Model:    rec.Model,
		Thinking: rec.Thinking,

		SessionID:   rec.SessionID,
		SessionFile: rec.SessionFile,
		SessionDir:  rec.SessionDir,
		NoSession:   rec.NoSession,

		Status:       protocol.Status(rec.Status),
		StartedAt:    rec.SpawnedAt,
		LastActivity: rec.LastActivity,
		ExitedAt:     rec.ExitedAt,
		ExitCode:     rec.ExitCode,
		ExitSignal:   rec.ExitSignal,

		ExecutorSelector: rec.ExecutorSelector,
		WorkspaceMode:    rec.WorkspaceMode,

		MaxDepth:    rec.MaxDepth,
		MaxCost:     rec.MaxCost,
		MaxChildren: rec.MaxChildren,

		ResumeSession:      rec.Config.ResumeSession,
		ForkSession:        rec.Config.ForkSession,
		Tools:              rec.Config.Tools,
		NoTools:            rec.Config.NoTools,
		NoBuiltinTools:     rec.Config.NoBuiltinTools,
		Extensions:         rec.Config.Extensions,
		NoExtensions:       rec.Config.NoExtensions,
		Skills:             rec.Config.Skills,
		NoSkills:           rec.Config.NoSkills,
		SkillsDirs:         rec.Config.SkillsDirs,
		MCPConfig:          rec.Config.MCPConfig,
		MCPServers:         rec.Config.MCPServers,
		NoMCP:              rec.Config.NoMCP,
		PromptTemplates:    rec.Config.PromptTemplates,
		NoPromptTemplates:  rec.Config.NoPromptTemplates,
		Themes:             rec.Config.Themes,
		NoThemes:           rec.Config.NoThemes,
		NoContextFiles:     rec.Config.NoContextFiles,
		SystemPrompt:       rec.Config.SystemPrompt,
		AppendSystemPrompt: rec.Config.AppendSystemPrompt,
		Verbose:            rec.Config.Verbose,
		PiBinary:           rec.Config.PiBinary,
		ExtraArgs:          rec.Config.ExtraArgs,
		RecordRequests:     rec.Config.RecordRequests,

		Labels: rec.Labels,
	}
}
