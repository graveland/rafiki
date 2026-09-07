// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound means the skill does not exist. It is an ANSWER: callers may
// turn it into a 404/NotFound. Any other error means "I could not check",
// which must surface as an internal error, never as "no such skill".
var ErrNotFound = errors.New("skill not found")

// ErrSourceConflict means the (namespace, name) is held by an ENABLED row
// the caller is not replacing. Two shapes reach it: an Upsert that would
// rewrite an enabled row's source — an upsert refreshes content, it does not
// take a name away from another source — and a SetEnabled that would create a
// second enabled row under one name (the partial unique index fires). For
// the enable case the escape is Delete (the startup sync reinserts core
// content) or disabling the incumbent first.
var ErrSourceConflict = errors.New("skill name is held by an enabled row of another source")

// CoreSource is the reserved provenance value for rows owned by the daemon's
// startup sync of its embedded corpus. Nothing else may write it, and the
// sync must never touch a row carrying any other value.
const CoreSource = "rafiki-core"

// DefaultNamespace is the namespace holding rafiki's own core skills and an
// operator's hand-written ones.
const DefaultNamespace = "rafiki"

// Record is one row of conversations.skills.
type Record struct {
	ID          string
	Namespace   string
	Name        string
	Description string
	Body        string
	Source      string
	OwnerUserID string // empty means global
	// ShadowedCoreVersion is the rafiki version whose core skill this row
	// replaced, or empty when it replaces nothing.
	ShadowedCoreVersion string
	Enabled             bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Meta projects a Record onto the inventory type the runtime consumes. The
// body is deliberately not carried: an inventory is a handful of lines and a
// body is a document, so bodies are fetched on the turn a model asks.
func (r Record) Meta() SkillMeta {
	return SkillMeta{
		Name:        r.Name,
		Description: r.Description,
		Namespace:   r.Namespace,
		Inline:      true,
	}
}

// Store is the skills backend. The Postgres implementation is pkg/skillsdb;
// this interface stays here so pkg/skills remains pgx-free.
type Store interface {
	// List returns rows ordered by (namespace, name). enabledOnly restricts
	// to enabled rows, which is what every read on the agent path wants.
	List(ctx context.Context, enabledOnly bool) ([]Record, error)

	// Get returns one row by namespace and name, enabled or not.
	// Returns ErrNotFound when there is no such row. When an override leaves
	// two rows under one name, Get deterministically returns the enabled one
	// (or the newest, when every row under the name is disabled) — reads on the
	// agent path still go through List(enabledOnly=true).
	Get(ctx context.Context, namespace, name string) (Record, error)

	// Upsert creates or replaces the row at (namespace, name). It never
	// changes an existing row's enabled flag: re-importing content must not
	// silently re-enable something an operator switched off. Upserting over an
	// ENABLED row of a different source returns ErrSourceConflict — a name
	// belongs to its source until the row is disabled or deleted. Refreshing
	// the SAME source over an enabled row is the normal content-replace path
	// and succeeds.
	Upsert(ctx context.Context, r Record) (Record, error)

	// SetEnabled flips one name's enabled flag: enable targets the disabled
	// row, disable the enabled one — the override state puts both under one
	// name, and enabling a name that still carries an enabled row (an override
	// not yet switched off) conflicts at the partial unique index and fails.
	// That conflict is ErrSourceConflict, not a raw driver error. Returns
	// ErrNotFound when the name has no row to flip: it is absent, or every row
	// is already in the requested state.
	SetEnabled(ctx context.Context, namespace, name string, enabled bool) error

	// Delete hard-deletes every row under the name — in the override state
	// that is two rows, and the disabled one has no other management surface.
	// Returns ErrNotFound when the name is absent. Unlike users, there is no
	// attribution history to preserve — nothing joins back to a skill row
	// after the fact.
	Delete(ctx context.Context, namespace, name string) error

	// ReplaceNamespaceSource makes (namespace, source) hold exactly want:
	// rows in want are upserted, and rows carrying that same namespace AND
	// source that are absent from want are deleted. Rows with a different
	// source are never touched, which is what keeps the core sync from
	// pruning an operator's content.
	ReplaceNamespaceSource(ctx context.Context, namespace, source string, want []Record) error
}
