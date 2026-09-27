// SPDX-License-Identifier: Apache-2.0

// Package gitpymodules is the domain type and store interface for pymodule
// git sources: owner-scoped (url, ref) pointers to external git content that
// the pymodule tool surface addresses as a `repo` scope. The Postgres
// implementation is pkg/gitpymodulesdb; this package stays pgx-free, same
// split as pkg/pymodules/pkg/pymodulesdb.
package gitpymodules

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrNotFound means the git source does not exist for the given owner.
var ErrNotFound = errors.New("git pymodule source not found")

// ErrReservedName rejects "local": that name is the sentinel every pymodule
// tool uses for the built-in blob store, so a git source registered under it
// would make repo="local" ambiguous between the two stores.
var ErrReservedName = errors.New(`"local" is reserved for the built-in pymodule store`)

// GitSourceRecord is one row of conversations.pymodule_git_sources.
type GitSourceRecord struct {
	ID          int64
	OwnerUserID string // empty means unattributed, same convention as pymodules.Record
	Name        string // the value used as the `repo` argument everywhere else
	URL         string
	Ref         string
	CreatedAt   time.Time
}

// Store is the git-source backend. The Postgres implementation is
// pkg/gitpymodulesdb; this interface stays here so pkg/gitpymodules remains
// pgx-free, same split as pymodules.Store/pymodulesdb.
type Store interface {
	// Put registers (or repoints) the source named name for ownerUserID --
	// ownerUserID empty means unattributed. A git source registration is a
	// pointer to repoint, not an append-only snippet history, so Put for an
	// existing (owner, name) updates url/ref in place rather than inserting
	// a new row. Put rejects name == "local" (ErrReservedName) and any other
	// name ValidateName refuses; every implementation must apply it before
	// touching the store.
	Put(ctx context.Context, ownerUserID, name, url, ref string) (GitSourceRecord, error)

	// List returns ownerUserID's sources ordered by name. ownerUserID empty
	// returns only OTHER unattributed rows -- never another owner's, and
	// never every owner's, same rule as pymodules.Store.List.
	List(ctx context.Context, ownerUserID string) ([]GitSourceRecord, error)

	// Delete removes ownerUserID's source named name outright (there is no
	// versioning to soft-delete). Returns ErrNotFound when no such row
	// exists; nothing is written in that case.
	Delete(ctx context.Context, ownerUserID, name string) error
}

// validNameRe is a single safe path segment, not a Python identifier: a git
// source's name only ever becomes a directory under the executor's checkout
// cache and the value of the `repo` argument. Python never imports it — the
// checkout's CONTENTS go on PYTHONPATH — so "review-swarm" is fine. No "."
// (so no "..", hidden directories or extension-shaped names), no leading "-"
// (nothing a subprocess could parse as an option), and no ":" (`--pymodule
// <repo>:<script>` splits on it).
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// ValidateName is the name rule every Store.Put must enforce, and every
// consumption point that turns a repo name into a path re-checks: "local" is
// reserved for the blob store, and everything else must be a 1-64 character
// validNameRe segment.
func ValidateName(name string) error {
	if name == "local" {
		return ErrReservedName
	}
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("git source name must be 1-64 characters, got %d", len(name))
	}
	if !validNameRe.MatchString(name) {
		return fmt.Errorf("git source name %q may only contain letters, digits, \"_\" and \"-\", and must not start with \"-\"", name)
	}
	return nil
}
