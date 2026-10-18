// SPDX-License-Identifier: Apache-2.0

// Package reposdb is the Postgres implementation of
// repos.Store. It is the only package that may touch pgx on the
// repo path -- see pkg/repos.
package reposdb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/repos"
)

// NewPostgresStore creates a repos.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) repos.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

// selectCols is every column a Record carries, in scan order. The three
// refresh columns are NULL-able, so each is scanned into a pointer: an absent
// commit is "never resolved", not an empty commit, and an absent interval is
// "on demand", not a zero schedule.
const selectCols = `id, COALESCE(owner_user_id::text, ''), name, url, ref,
	commit_sha, refresh_interval, resolved_at, created_at`

func scanRecord(row pgx.Row) (repos.Record, error) {
	var (
		r          repos.Record
		commit     *string
		interval   *time.Duration
		resolvedAt *time.Time
	)
	err := row.Scan(&r.ID, &r.OwnerUserID, &r.Name, &r.URL, &r.Ref,
		&commit, &interval, &resolvedAt, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	if commit != nil {
		r.Commit = *commit
	}
	if interval != nil {
		r.RefreshInterval = *interval
	}
	if resolvedAt != nil {
		r.ResolvedAt = *resolvedAt
	}
	return r, nil
}

// ownerArg turns an empty ownerUserID into a real SQL NULL rather than an
// empty-string value -- required for IS NOT DISTINCT FROM below to treat
// every unattributed row as belonging to one shared "no owner" bucket, and
// for the unique index's NULLS NOT DISTINCT to keep the unattributed bucket
// one identity (not one row per Put).
func ownerArg(ownerUserID string) any {
	if ownerUserID == "" {
		return nil
	}
	return ownerUserID
}

// Put validates the name (repos.ValidateName: "local" reserved, plus
// pymodules.ValidName) and upserts. Unlike pymodules there is no versioning:
// a repo registration is a pointer to repoint, so an existing
// (owner, name) has url/ref updated in place. The unique index on
// (owner_user_id, name) is NULLS NOT DISTINCT -- SQL's default treats NULLs
// as distinct, which would let two unattributed rows for one name coexist
// and the ON CONFLICT would never fire for ownerUserID "".
//
// The resolved state (commit_sha, refresh_interval, resolved_at) is
// deliberately absent from both the INSERT column list and the DO UPDATE: a
// repoint keeps whatever the last refresh recorded until the next refresh
// resolves the new ref, and a fresh row starts unresolved.
func (s *pgStore) Put(ctx context.Context, ownerUserID, name, url, ref string) (repos.Record, error) {
	if err := repos.ValidateName(name); err != nil {
		return repos.Record{}, err
	}
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`INSERT INTO conversations.repos (owner_user_id, name, url, ref)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (owner_user_id, name)
		 DO UPDATE SET url = EXCLUDED.url, ref = EXCLUDED.ref
		 RETURNING `+selectCols,
		ownerArg(ownerUserID), name, url, ref))
	if err != nil {
		return repos.Record{}, fmt.Errorf("put repo: %w", err)
	}
	return r, nil
}

// List returns ownerUserID's sources ordered by name, owner-scoped with IS
// NOT DISTINCT FROM so the unattributed bucket (ownerUserID == "") is one
// shared identity: SQL NULL = NULL is never true, so "=" would make every
// unattributed caller see zero rows instead of every other unattributed row.
func (s *pgStore) List(ctx context.Context, ownerUserID string) ([]repos.Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.repos
		 WHERE owner_user_id IS NOT DISTINCT FROM $1
		 ORDER BY name`,
		ownerArg(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()
	var out []repos.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes the row outright -- there is no version history to
// soft-delete, unlike pymodules. RowsAffected == 0 means no such row existed
// -> repos.ErrNotFound, nothing written. Owner-scoped like every
// other operation: A deleting "shared" must not touch B's copy.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM conversations.repos
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return repos.ErrNotFound
	}
	return nil
}

// SetResolved records the commit a refresh fanned out and when. The timestamp
// is written even when the commit is unchanged: it is what the scheduler
// anchors the next fire on, so a refresh that found nothing new still resets
// the clock. An empty commit stores NULL rather than an empty string — "never
// resolved" is the absence of an answer, not an empty one.
func (s *pgStore) SetResolved(ctx context.Context, ownerUserID, name, commit string, at time.Time) error {
	var commitArg any
	if commit != "" {
		commitArg = commit
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.repos
		 SET commit_sha = $3, resolved_at = $4
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name, commitArg, at)
	if err != nil {
		return fmt.Errorf("set repo resolved commit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return repos.ErrNotFound
	}
	return nil
}

// SetRefreshInterval sets (or clears, for d == 0) the source's opt-in
// schedule. The floor is enforced here as well as by the table's CHECK
// constraint, so the caller gets the named sentinel instead of a constraint
// violation it would have to parse. NULL means on demand — a stored zero
// interval would be a schedule that fires constantly, not "never".
func (s *pgStore) SetRefreshInterval(ctx context.Context, ownerUserID, name string, d time.Duration) error {
	if d < 0 || (d > 0 && d < repos.MinRefreshInterval) {
		return repos.ErrIntervalTooShort
	}
	var intervalArg any
	if d > 0 {
		intervalArg = d
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.repos
		 SET refresh_interval = $3
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name, intervalArg)
	if err != nil {
		return fmt.Errorf("set repo refresh interval: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return repos.ErrNotFound
	}
	return nil
}

// ListAll returns every owner's rows, ordered by (owner, name) with the
// unattributed bucket first. It is the scheduler's read — the only caller that
// legitimately needs the whole table — and is never reachable from a request
// path, where every read is owner-scoped.
func (s *pgStore) ListAll(ctx context.Context) ([]repos.Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.repos
		 ORDER BY owner_user_id NULLS FIRST, name`)
	if err != nil {
		return nil, fmt.Errorf("list all repos: %w", err)
	}
	defer rows.Close()
	var out []repos.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all repos: %w", err)
	}
	return out, nil
}

// compile-time pin: the Postgres store implements every method the interface
// names. A new Store method is a build failure here rather than a nil-method
// panic at the first call.
var _ repos.Store = (*pgStore)(nil)
