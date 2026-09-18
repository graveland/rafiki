// SPDX-License-Identifier: Apache-2.0

// Package pymodulesdb is the Postgres implementation of pymodules.Store. It
// is the only package that may touch pgx on the pymodule path -- see
// pkg/pymodules.
package pymodulesdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// NewPostgresStore creates a pymodules.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) pymodules.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

const selectCols = `id, COALESCE(owner_user_id::text, ''), name, code, description, created_at`

func scanRecord(row pgx.Row) (pymodules.Record, error) {
	var r pymodules.Record
	err := row.Scan(&r.ID, &r.OwnerUserID, &r.Name, &r.Code, &r.Description, &r.CreatedAt)
	return r, err
}

// ownerArg turns an empty ownerUserID into a real SQL NULL rather than an
// empty-string value -- required for IS NOT DISTINCT FROM below to treat
// every unattributed row as belonging to one shared "no owner" bucket.
func ownerArg(ownerUserID string) any {
	if ownerUserID == "" {
		return nil
	}
	return ownerUserID
}

// Put always inserts -- nothing is ever updated or deleted. Versioning is
// the identity column: two Puts under the same (owner, name) leave TWO rows,
// and "latest" is decided by List's ORDER BY id DESC, never by an UPDATE.
func (s *pgStore) Put(ctx context.Context, ownerUserID, name, code, description string) (pymodules.Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`INSERT INTO conversations.pymodules (owner_user_id, name, code, description)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+selectCols,
		ownerArg(ownerUserID), name, code, description))
	if err != nil {
		return pymodules.Record{}, fmt.Errorf("put pymodule: %w", err)
	}
	return r, nil
}

// List returns the latest row per name for ownerUserID. IS NOT DISTINCT
// FROM (not "=") is required because SQL NULL = NULL is never true: without
// it, every unattributed caller (ownerUserID == "") would match ZERO rows
// instead of every other unattributed row. Live rows only: deleted_at IS
// NULL. Delete stamps every version of the name, so there is no older live
// row to resurrect.
func (s *pgStore) List(ctx context.Context, ownerUserID string) ([]pymodules.Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (name) `+selectCols+`
		 FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL
		 ORDER BY name, id DESC`,
		ownerArg(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("list pymodules: %w", err)
	}
	defer rows.Close()
	var out []pymodules.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pymodule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get mirrors List's per-name rule for one name: latest live row by id,
// owner-scoped with the same IS NOT DISTINCT FROM semantics for the
// unattributed bucket. pgx.ErrNoRows becomes pymodules.ErrNotFound -- an
// ANSWER, not an error of the store.
func (s *pgStore) Get(ctx context.Context, ownerUserID, name string) (pymodules.Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2 AND deleted_at IS NULL
		 ORDER BY id DESC
		 LIMIT 1`,
		ownerArg(ownerUserID), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return pymodules.Record{}, pymodules.ErrNotFound
	}
	if err != nil {
		return pymodules.Record{}, fmt.Errorf("get pymodule: %w", err)
	}
	return r, nil
}

// Delete soft-deletes every version of name by stamping deleted_at on all
// live rows -- the only mutation any pymodule row ever undergoes; Put is
// insert-only. RowsAffected == 0 means no live row existed (unknown name or
// already deleted) -> ErrNotFound, nothing written. A Put that commits after
// this statement is a new live row and restores the name; a save racing a
// delete may be stamped hidden -- disclosed, identical either ordering.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.pymodules SET deleted_at = now()
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2 AND deleted_at IS NULL`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return fmt.Errorf("delete pymodule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pymodules.ErrNotFound
	}
	return nil
}
