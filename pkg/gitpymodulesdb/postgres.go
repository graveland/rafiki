// SPDX-License-Identifier: Apache-2.0

// Package gitpymodulesdb is the Postgres implementation of
// gitpymodules.Store. It is the only package that may touch pgx on the
// git-source path -- see pkg/gitpymodules.
package gitpymodulesdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// NewPostgresStore creates a gitpymodules.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) gitpymodules.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

const selectCols = `id, COALESCE(owner_user_id::text, ''), name, url, ref, created_at`

func scanRecord(row pgx.Row) (gitpymodules.GitSourceRecord, error) {
	var r gitpymodules.GitSourceRecord
	err := row.Scan(&r.ID, &r.OwnerUserID, &r.Name, &r.URL, &r.Ref, &r.CreatedAt)
	return r, err
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

// Put validates the name (gitpymodules.ValidateName: "local" reserved, plus
// pymodules.ValidName) and upserts. Unlike pymodules there is no versioning:
// a git source registration is a pointer to repoint, so an existing
// (owner, name) has url/ref updated in place. The unique index on
// (owner_user_id, name) is NULLS NOT DISTINCT -- SQL's default treats NULLs
// as distinct, which would let two unattributed rows for one name coexist
// and the ON CONFLICT would never fire for ownerUserID "".
func (s *pgStore) Put(ctx context.Context, ownerUserID, name, url, ref string) (gitpymodules.GitSourceRecord, error) {
	if err := gitpymodules.ValidateName(name); err != nil {
		return gitpymodules.GitSourceRecord{}, err
	}
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`INSERT INTO conversations.pymodule_git_sources (owner_user_id, name, url, ref)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (owner_user_id, name)
		 DO UPDATE SET url = EXCLUDED.url, ref = EXCLUDED.ref
		 RETURNING `+selectCols,
		ownerArg(ownerUserID), name, url, ref))
	if err != nil {
		return gitpymodules.GitSourceRecord{}, fmt.Errorf("put git pymodule source: %w", err)
	}
	return r, nil
}

// List returns ownerUserID's sources ordered by name, owner-scoped with IS
// NOT DISTINCT FROM so the unattributed bucket (ownerUserID == "") is one
// shared identity: SQL NULL = NULL is never true, so "=" would make every
// unattributed caller see zero rows instead of every other unattributed row.
func (s *pgStore) List(ctx context.Context, ownerUserID string) ([]gitpymodules.GitSourceRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.pymodule_git_sources
		 WHERE owner_user_id IS NOT DISTINCT FROM $1
		 ORDER BY name`,
		ownerArg(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("list git pymodule sources: %w", err)
	}
	defer rows.Close()
	var out []gitpymodules.GitSourceRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan git pymodule source: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes the row outright -- there is no version history to
// soft-delete, unlike pymodules. RowsAffected == 0 means no such row existed
// -> gitpymodules.ErrNotFound, nothing written. Owner-scoped like every
// other operation: A deleting "shared" must not touch B's copy.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM conversations.pymodule_git_sources
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return fmt.Errorf("delete git pymodule source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return gitpymodules.ErrNotFound
	}
	return nil
}
