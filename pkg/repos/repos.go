// SPDX-License-Identifier: Apache-2.0

// Package repos is the domain type and store interface for pymodule
// repos: owner-scoped (url, ref) pointers to external git content that
// the pymodule tool surface addresses as a `repo` scope. The Postgres
// implementation is pkg/reposdb; this package stays pgx-free, same
// split as pkg/pymodules/pkg/pymodulesdb.
package repos

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrNotFound means the repo does not exist for the given owner.
var ErrNotFound = errors.New("repo not found")

// ErrReservedName rejects "local": that name is the sentinel every pymodule
// tool uses for the built-in blob store, so a repo registered under it
// would make repo="local" ambiguous between the two stores.
var ErrReservedName = errors.New(`"local" is reserved for the built-in pymodule store`)

// ErrIntervalTooShort refuses a schedule shorter than MinRefreshInterval. Zero
// is the "on demand" answer and is always accepted; anything else must be a
// real schedule, because a timer firing every few seconds would re-clone the
// source faster than git can serve it.
var ErrIntervalTooShort = errors.New("refresh interval must be at least 1m")

// MinRefreshInterval is the shortest refresh schedule a store accepts, and the
// same floor the conversations.repos CHECK constraint enforces.
const MinRefreshInterval = time.Minute

// Record is one row of conversations.repos.
type Record struct {
	ID          int64
	OwnerUserID string // empty means unattributed, same convention as pymodules.Record
	Name        string // the value used as the `repo` argument everywhere else
	URL         string
	Ref         string

	// Commit is the commit the last successful refresh resolved Ref to. Empty
	// means the source has never been resolved, which is not the same as "no
	// commit" — nothing is known about it yet.
	Commit string
	// RefreshInterval is the opt-in schedule: 0 means on demand (never).
	RefreshInterval time.Duration
	// ResolvedAt is when Commit was resolved; the zero time means never.
	ResolvedAt time.Time

	CreatedAt time.Time
}

// Store is the repo backend. The Postgres implementation is
// pkg/reposdb; this interface stays here so pkg/repos remains
// pgx-free, same split as pymodules.Store/pymodulesdb.
type Store interface {
	// Put registers (or repoints) the source named name for ownerUserID --
	// ownerUserID empty means unattributed. A repo registration is a
	// pointer to repoint, not an append-only snippet history, so Put for an
	// existing (owner, name) updates url/ref in place rather than inserting
	// a new row. Put rejects name == "local" (ErrReservedName) and any other
	// name ValidateName refuses; every implementation must apply it before
	// touching the store.
	//
	// A repoint leaves the resolved state alone: Commit, RefreshInterval and
	// ResolvedAt describe the PREVIOUS ref until the next refresh resolves the
	// new one, so Put must not clear or rewrite them.
	Put(ctx context.Context, ownerUserID, name, url, ref string) (Record, error)

	// List returns ownerUserID's sources ordered by name. ownerUserID empty
	// returns only OTHER unattributed rows -- never another owner's, and
	// never every owner's, same rule as pymodules.Store.List.
	List(ctx context.Context, ownerUserID string) ([]Record, error)

	// Delete removes ownerUserID's source named name outright (there is no
	// versioning to soft-delete). Returns ErrNotFound when no such row
	// exists; nothing is written in that case.
	Delete(ctx context.Context, ownerUserID, name string) error

	// SetResolved records the commit a refresh resolved the source's ref to and
	// when it did so. A commit resolved identically to the stored one is still
	// written: the timestamp is the scheduler's anchor for the next fire.
	// Returns ErrNotFound when no such row exists.
	SetResolved(ctx context.Context, ownerUserID, name, commit string, at time.Time) error

	// SetRefreshInterval sets the source's opt-in schedule. Zero stores NULL
	// (on demand, the default); a positive value below MinRefreshInterval is
	// refused with ErrIntervalTooShort. Returns ErrNotFound when no such row
	// exists.
	SetRefreshInterval(ctx context.Context, ownerUserID, name string, d time.Duration) error

	// ListAll returns every owner's rows ordered by (owner, name) — the whole
	// table, not one owner's slice. It exists for the scheduler, which arms one
	// timer per scheduled source regardless of who owns it; no request path may
	// call it, because every other read here is owner-scoped.
	ListAll(ctx context.Context) ([]Record, error)
}

// validNameRe is a single safe path segment, not a Python identifier: a repo's
// name only ever becomes a directory under the executor's checkout
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
		return fmt.Errorf("repo name must be 1-64 characters, got %d", len(name))
	}
	if !validNameRe.MatchString(name) {
		return fmt.Errorf("repo name %q may only contain letters, digits, \"_\" and \"-\", and must not start with \"-\"", name)
	}
	return nil
}
