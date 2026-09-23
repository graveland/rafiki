// SPDX-License-Identifier: Apache-2.0

// Package presets is the domain type and store interface for agent presets.
// The Postgres implementation is pkg/presetsdb; this package stays pgx-free
// (cmd/rafiki imports it).
package presets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound means the preset does not exist for the given owner.
var ErrNotFound = errors.New("preset not found")

const (
	KindFundi  = "fundi"
	KindClaude = "claude"
)

// Record is one row of conversations.presets.
type Record struct {
	ID                 int64
	OwnerUserID        string // "" = the unattributed bucket, never "global"
	Name               string
	Description        string
	Kind               string            // KindFundi | KindClaude
	Provider           string            // "" = unset (NULL)
	Model              string            // "" = unset (NULL)
	Thinking           string            // "" = unset (NULL)
	Executor           string            // "" = unset (NULL); a label selector
	Labels             map[string]string // never nil after a read
	Tools              []string          // TRI-STATE: nil = kind default (all), non-nil empty = none
	Skills             []string          // TRI-STATE, same rule
	MCPServers         []string          // TRI-STATE, same rule
	ContextFiles       *bool             // nil = default (on)
	SystemPrompt       string            // "" = unset (NULL)
	AppendSystemPrompt string            // "" = unset (NULL)
	MaxCost            *float64
	MaxDepth           *int
	MaxChildren        *int
	WrittenByChild     string     // "" = written by the operator
	DeletedAt          *time.Time // non-nil only on rows returned by History
	CreatedAt          time.Time
}

// Spec is the JSON shape a preset is written in: the CLI's `rafiki preset
// put -f` file and the preset_put tool's input. Pointer-to-slice keeps the
// tri-state: absent/null = unset, [] = none.
type Spec struct {
	Name               string            `json:"name,omitempty"`
	Description        string            `json:"description,omitempty"`
	Kind               string            `json:"kind,omitempty"`
	Provider           string            `json:"provider,omitempty"`
	Model              string            `json:"model,omitempty"`
	Thinking           string            `json:"thinking,omitempty"`
	Executor           string            `json:"executor,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	Tools              *[]string         `json:"tools,omitempty"`
	Skills             *[]string         `json:"skills,omitempty"`
	MCPServers         *[]string         `json:"mcp_servers,omitempty"`
	ContextFiles       *bool             `json:"context_files,omitempty"`
	SystemPrompt       string            `json:"system_prompt,omitempty"`
	AppendSystemPrompt string            `json:"append_system_prompt,omitempty"`
	MaxCost            *float64          `json:"max_cost,omitempty"`
	MaxDepth           *int              `json:"max_depth,omitempty"`
	MaxChildren        *int              `json:"max_children,omitempty"`
}

// ParseSpec decodes one JSON object with DisallowUnknownFields and rejects
// trailing data after it.
func ParseSpec(data []byte) (Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, err
	}
	// Anything after the one object -- another value, trailing garbage --
	// must fail; only whitespace may remain.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return Spec{}, err
		}
		return Spec{}, fmt.Errorf("unexpected data after JSON object")
	}
	return s, nil
}

// Record converts a Spec. Empty Kind becomes KindFundi. A non-nil
// *[]string becomes a NON-NIL copy (so `[]` stays "none"); a nil pointer
// stays nil. nil Labels become an empty map.
func (s Spec) Record() Record {
	r := Record{
		Name:               s.Name,
		Description:        s.Description,
		Kind:               s.Kind,
		Provider:           s.Provider,
		Model:              s.Model,
		Thinking:           s.Thinking,
		Executor:           s.Executor,
		Labels:             s.Labels,
		ContextFiles:       s.ContextFiles,
		SystemPrompt:       s.SystemPrompt,
		AppendSystemPrompt: s.AppendSystemPrompt,
		MaxCost:            s.MaxCost,
		MaxDepth:           s.MaxDepth,
		MaxChildren:        s.MaxChildren,
		Tools:              specSlice(s.Tools),
		Skills:             specSlice(s.Skills),
		MCPServers:         specSlice(s.MCPServers),
	}
	if r.Kind == "" {
		r.Kind = KindFundi
	}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	return r
}

// specSlice dereferences an optional slice into a tri-state plain slice: a
// nil pointer stays nil (the kind's default), a non-nil pointer becomes a
// NON-NIL copy, so an empty `[]` keeps meaning "none" after conversion.
func specSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(*p))
	copy(out, *p)
	return out
}

// SpecOf is the inverse of Spec.Record: nil slice -> nil pointer, non-nil
// (even empty) slice -> pointer to a copy. Empty Labels -> nil.
func SpecOf(r Record) Spec {
	s := Spec{
		Name:               r.Name,
		Description:        r.Description,
		Kind:               r.Kind,
		Provider:           r.Provider,
		Model:              r.Model,
		Thinking:           r.Thinking,
		Executor:           r.Executor,
		ContextFiles:       r.ContextFiles,
		SystemPrompt:       r.SystemPrompt,
		AppendSystemPrompt: r.AppendSystemPrompt,
		MaxCost:            r.MaxCost,
		MaxDepth:           r.MaxDepth,
		MaxChildren:        r.MaxChildren,
		Tools:              recordSlice(r.Tools),
		Skills:             recordSlice(r.Skills),
		MCPServers:         recordSlice(r.MCPServers),
	}
	if len(r.Labels) > 0 {
		s.Labels = r.Labels
	}
	return s
}

// recordSlice wraps a tri-state plain slice back into an optional pointer:
// nil (the kind's default) stays a nil pointer; a non-nil slice, even an
// empty one ("none"), becomes a pointer to a copy.
func recordSlice(xs []string) *[]string {
	if xs == nil {
		return nil
	}
	out := make([]string, len(xs))
	copy(out, xs)
	return &out
}

// validNameRe accepts 1-64 characters of the preset name grammar: a bare
// name or `<group>:<role>` -- each side starts alphanumerically and
// continues with alphanumerics, dots, underscores or dashes, and there is
// at most one colon.
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(:[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

// ValidName checks the preset name grammar so a name can never be confused
// with another selector on the resolve path.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("preset name must be 1-64 characters, got %d", len(name))
	}
	if !validNameRe.MatchString(name) {
		return fmt.Errorf("preset name %q must be <name> or <group>:<role> (letters, digits, dot, underscore, dash, at most one colon)", name)
	}
	return nil
}

// Group returns the `<group>:` prefix of name including the colon, or ""
// when name has no colon. Group("local:reviewer") == "local:".
func Group(name string) string {
	if i := strings.Index(name, ":"); i >= 0 {
		return name[:i+1]
	}
	return ""
}

// thinkingLevels are the values conversations.presets' CHECK accepts and
// the routing layer understands, in ascending intensity.
var thinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// Validate checks everything the database's CHECKs check, plus what they
// cannot, so a bad preset fails with a readable message before the INSERT:
//   - ValidName(r.Name)
//   - r.Kind is KindFundi or KindClaude
//   - r.Thinking is "" or one of off|minimal|low|medium|high|xhigh
//   - when r.Kind == KindClaude: Thinking == "", Tools == nil, Skills == nil,
//     MCPServers == nil, ContextFiles == nil, SystemPrompt == "" -- the error
//     names the first offending field: `field "tools" does not apply to kind "claude"`
//   - MaxCost/MaxDepth/MaxChildren, when non-nil, are >= 0
//   - every Labels key is non-empty and does not start with "rafiki/"
func Validate(r Record) error {
	if err := ValidName(r.Name); err != nil {
		return err
	}
	if r.Kind != KindFundi && r.Kind != KindClaude {
		return fmt.Errorf("field %q must be %q or %q, got %q", "kind", KindFundi, KindClaude, r.Kind)
	}
	if r.Thinking != "" && !thinkingLevels[r.Thinking] {
		return fmt.Errorf("field %q must be one of off|minimal|low|medium|high|xhigh, got %q", "thinking", r.Thinking)
	}
	if r.Kind == KindClaude {
		for _, bad := range []struct {
			field string
			set   bool
		}{
			{"thinking", r.Thinking != ""},
			{"tools", r.Tools != nil},
			{"skills", r.Skills != nil},
			{"mcp_servers", r.MCPServers != nil},
			{"context_files", r.ContextFiles != nil},
			{"system_prompt", r.SystemPrompt != ""},
		} {
			if bad.set {
				return fmt.Errorf("field %q does not apply to kind %q", bad.field, KindClaude)
			}
		}
	}
	if r.MaxCost != nil && *r.MaxCost < 0 {
		return fmt.Errorf("field %q must be >= 0, got %v", "max_cost", *r.MaxCost)
	}
	if r.MaxDepth != nil && *r.MaxDepth < 0 {
		return fmt.Errorf("field %q must be >= 0, got %d", "max_depth", *r.MaxDepth)
	}
	if r.MaxChildren != nil && *r.MaxChildren < 0 {
		return fmt.Errorf("field %q must be >= 0, got %d", "max_children", *r.MaxChildren)
	}
	for key := range r.Labels {
		if key == "" {
			return fmt.Errorf("field %q keys must be non-empty", "labels")
		}
		if strings.HasPrefix(key, "rafiki/") {
			return fmt.Errorf("field %q keys must not start with %q, got %q", "labels", "rafiki/", key)
		}
	}
	return nil
}

// Store: owner-scoped, append-only. No method takes a caller-supplied
// identity other than ownerUserID, which the daemon supplies.
type Store interface {
	// Put inserts a new row and never modifies an existing one. It ignores
	// r.ID, r.OwnerUserID, r.CreatedAt and r.DeletedAt.
	Put(ctx context.Context, ownerUserID string, r Record) (Record, error)
	// Get returns the latest live row for name, or ErrNotFound.
	Get(ctx context.Context, ownerUserID, name string) (Record, error)
	// List returns the latest live row per name whose name starts with
	// prefix ("" = all), ordered by name.
	List(ctx context.Context, ownerUserID, prefix string) ([]Record, error)
	// History returns every row for name, live or deleted, newest first,
	// or ErrNotFound when there are none.
	History(ctx context.Context, ownerUserID, name string) ([]Record, error)
	// Delete stamps deleted_at on every live row for name (an UPDATE, the
	// only mutation a preset row undergoes), or ErrNotFound when none is live.
	Delete(ctx context.Context, ownerUserID, name string) error
}
