// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound means the skill does not exist. It is an ANSWER: callers may
// turn it into a 404/NotFound. Any other error means "I could not check",
// which must surface as an internal error, never as "no such skill".
var ErrNotFound = errors.New("skill not found")

// NamespaceHeldError: a skill set tried to write a namespace that another
// source already holds in the same layer for the same owner. A namespace is
// claimed whole by its source, so the writer refuses rather than mixing two
// sources' skills into one namespace nobody owns.
type NamespaceHeldError struct{ Namespace, Holder string }

func (e *NamespaceHeldError) Error() string {
	return fmt.Sprintf("namespace %q is already provided by %s", e.Namespace, e.Holder)
}

// SetKey names one skill set: every row with this owner and source. Source is
// ownership — a writer may only create, replace and delete rows carrying its
// own source marker. An empty OwnerUserID is the global layer.
type SetKey struct{ OwnerUserID, Source string }

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

	// HasFiles reports whether the row has any supporting files, without
	// loading them. List populates it; it is what lets an inventory say "this
	// skill has more than its SKILL.md" without a second query per row.
	HasFiles bool
	// Files are the skill's supporting files, sorted by path. Populated only by
	// Get and by callers of PutSet — a List deliberately carries none, because
	// an inventory is a handful of lines and a file set is a directory.
	Files []File

	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Meta projects a Record onto the inventory type the runtime consumes. The
// body and files are deliberately not carried: an inventory is a handful of
// lines and a body is a document, so both are fetched on the turn a model asks.
func (r Record) Meta() SkillMeta {
	return SkillMeta{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Namespace:   r.Namespace,
		Inline:      true,
		HasFiles:    r.HasFiles,
	}
}

// Store is the skills backend. The Postgres implementation is pkg/skillsdb;
// this interface stays here so pkg/skills remains pgx-free.
type Store interface {
	// List returns every owner's rows ordered by (namespace, name). Files are
	// nil; HasFiles is populated. enabledOnly restricts to enabled rows, which
	// is what every read on the agent path wants.
	List(ctx context.Context, enabledOnly bool) ([]Record, error)

	// Get returns one row by id, with its Files sorted by path. Returns
	// ErrNotFound when there is no such row.
	Get(ctx context.Context, id string) (Record, error)

	// Files returns the supporting files of several skills in one query, keyed
	// by skill id and sorted by path. A skill with no files is absent from the
	// map, so a caller's lookup yields nil.
	Files(ctx context.Context, ids []string) (map[string][]File, error)

	// PutSet makes key hold exactly want: rows in want are written (content
	// only — an existing row's enabled flag is never changed), rows carrying
	// that same source and owner that are absent from want are deleted, and
	// blobs this write stopped referencing are swept. It returns changed=false
	// when the stored set is already byte-identical, having written nothing.
	//
	// A namespace held in the same layer by a different source is refused with
	// *NamespaceHeldError, and nothing is written: the write never upserts over
	// another source's row.
	PutSet(ctx context.Context, key SetKey, want []Record) (changed bool, err error)

	// SetEnabled flips one row's enabled flag by id. Returns ErrNotFound when
	// there is no such row.
	SetEnabled(ctx context.Context, id string, enabled bool) error

	// Delete hard-deletes one row by id, and its file rows by cascade. Returns
	// ErrNotFound when there is no such row. Unlike users, there is no
	// attribution history to preserve — nothing joins back to a skill row after
	// the fact.
	Delete(ctx context.Context, id string) error
}
