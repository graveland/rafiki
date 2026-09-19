// SPDX-License-Identifier: Apache-2.0

// Package pymodules is the domain type and store interface for the pymodule
// store: reusable, owner-scoped, versioned Python snippets. The Postgres
// implementation is pkg/pymodulesdb; this package stays pgx-free.
package pymodules

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrNotFound means the module does not exist for the given owner.
var ErrNotFound = errors.New("pymodule not found")

// LocalRepo is the repo scope name of the owner's own blob store -- the only
// scope pymodule_put/delete/get can address. Every other repo value names a
// git source, whose content is read-only through pymodule_run (its own
// history is the only mutation path) and which these tools must reject.
// Defined here, next to the store it names a scope of, so the tool layer,
// the daemon adapters and the CLI all quote one constant instead of three
// copies of the literal "local".
const LocalRepo = "local"

// Record is one row of conversations.pymodules.
type Record struct {
	ID          int64
	OwnerUserID string // empty means unattributed, never "global"
	Name        string
	Code        string
	Description string
	CreatedAt   time.Time
}

// Store is the pymodule backend. The Postgres implementation is
// pkg/pymodulesdb; this interface stays here so pkg/pymodules remains
// pgx-free.
type Store interface {
	// Put always inserts a new row and never modifies an existing one.
	// ownerUserID empty means unattributed. A delete stamps deleted_at on
	// every version of the name; it never rewrites code.
	Put(ctx context.Context, ownerUserID, name, code, description string) (Record, error)

	// List returns the latest row per name for ownerUserID (MAX(id) per
	// name), ordered by name. ownerUserID empty returns only OTHER
	// unattributed rows -- never another owner's, and never every owner's.
	List(ctx context.Context, ownerUserID string) ([]Record, error)

	// Get returns the latest live row for ownerUserID's name -- MAX(id) among
	// that name's rows with deleted_at IS NULL. Returns ErrNotFound when no
	// live row exists: unknown name, or every version deleted. A Get after a
	// delete finds nothing; a later Put under the same name restores it.
	Get(ctx context.Context, ownerUserID, name string) (Record, error)

	// Delete soft-deletes every version of name by stamping deleted_at on all
	// live rows. This is the ONLY mutation a pymodule row ever undergoes --
	// Put is insert-only -- and it is why List's plain deleted_at IS NULL
	// filter cannot resurrect an older version: a delete leaves no live row
	// behind. Returns ErrNotFound when no live row exists (unknown name or
	// already deleted); nothing is written in that case. A later Put under
	// the same name is a fresh live row and restores it.
	Delete(ctx context.Context, ownerUserID string, name string) error
}

// validNameRe is deliberately a bare Python identifier, not just a safe path
// segment: a module name becomes BOTH `<name>.py` on disk on every executor
// it syncs to, AND the argument to `import <name>` in a script. Restricting
// to identifier characters satisfies both constraints in one check and
// forbids "/", "\", "..", and empty by construction.
var validNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name is safe to use as a pymodule name. Checked
// at every write (pymodule_put) and re-checked at every consumption point
// that turns a name into a path segment (the sync receiver, pymodule_run) --
// each call site validates independently, never trusting an earlier check.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("pymodule name must be 1-64 characters, got %d", len(name))
	}
	if !validNameRe.MatchString(name) {
		return fmt.Errorf("pymodule name %q must be a bare Python identifier (letters, digits, underscore, not starting with a digit)", name)
	}
	return nil
}
