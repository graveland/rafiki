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
	// Returns ErrNotFound when there is no such row.
	Get(ctx context.Context, namespace, name string) (Record, error)

	// Upsert creates or replaces the row at (namespace, name). It never
	// changes an existing row's enabled flag: re-importing content must not
	// silently re-enable something an operator switched off.
	Upsert(ctx context.Context, r Record) (Record, error)

	// SetEnabled flips one row's enabled flag. Returns ErrNotFound when
	// there is no such row.
	SetEnabled(ctx context.Context, namespace, name string, enabled bool) error

	// Delete hard-deletes one row. Returns ErrNotFound when absent. Unlike
	// users, there is no attribution history to preserve — nothing joins back
	// to a skill row after the fact.
	Delete(ctx context.Context, namespace, name string) error

	// ReplaceNamespaceSource makes (namespace, source) hold exactly want:
	// rows in want are upserted, and rows carrying that same namespace AND
	// source that are absent from want are deleted. Rows with a different
	// source are never touched, which is what keeps the core sync from
	// pruning an operator's content.
	ReplaceNamespaceSource(ctx context.Context, namespace, source string, want []Record) error
}
