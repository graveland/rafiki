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
	"time"

	"go.graveland.dev/rafiki/pkg/pymodules"
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
	// a new row. Put rejects name == "local" (ErrReservedName) and otherwise
	// requires pymodules.ValidName -- see ValidateName, which every
	// implementation must apply before touching the store.
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

// ValidateName is the name rule every Store.Put must enforce: "local" is
// reserved for the blob store, and everything else must pass
// pymodules.ValidName, because a git source's name becomes the value of the
// `repo` argument across the whole pymodule tool surface.
func ValidateName(name string) error {
	if name == "local" {
		return ErrReservedName
	}
	return pymodules.ValidName(name)
}
