// SPDX-License-Identifier: Apache-2.0

// Package reposdb is the Postgres implementation of
// repos.Store. It is the only package that may touch pgx on the
// repo path -- see pkg/repos.
package reposdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/repos"
)

// NewPostgresStore creates a repos.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) repos.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

const selectCols = `id, COALESCE(owner_user_id::text, ''), name, url, ref, created_at`

func scanRecord(row pgx.Row) (repos.Record, error) {
	var r repos.Record
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

// Put validates the name (repos.ValidateName: "local" reserved, plus
// pymodules.ValidName) and upserts. Unlike pymodules there is no versioning:
// a repo registration is a pointer to repoint, so an existing
// (owner, name) has url/ref updated in place. The unique index on
// (owner_user_id, name) is NULLS NOT DISTINCT -- SQL's default treats NULLs
// as distinct, which would let two unattributed rows for one name coexist
// and the ON CONFLICT would never fire for ownerUserID "".
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
