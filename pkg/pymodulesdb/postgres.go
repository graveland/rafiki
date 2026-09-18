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
// instead of every other unattributed row. The tombstone filter sits OUTSIDE
// the DISTINCT ON subquery: inside it, a deleted name's older live rows would
// resurface as the "latest" and a delete would resurrect v1.
func (s *pgStore) List(ctx context.Context, ownerUserID string) ([]pymodules.Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, owner_user_id, name, code, description, created_at FROM (
		SELECT DISTINCT ON (name) id, COALESCE(owner_user_id::text, '') AS owner_user_id, name, code, description, created_at, deleted_at
		FROM conversations.pymodules
		WHERE owner_user_id IS NOT DISTINCT FROM $1
		ORDER BY name, id DESC
	) latest WHERE deleted_at IS NULL ORDER BY name`, ownerArg(ownerUserID))
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

// Delete appends a tombstone row for name's latest live version. The
// existence probe reads the LATEST row overall (tombstones included): a name
// whose latest row is already a tombstone reports ErrNotFound rather than
// stacking another tombstone, and an unknown name reports ErrNotFound without
// inserting anything. Two concurrent deletes may both pass the probe and both
// insert -- harmless, the name stays deleted either way.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	var live bool
	err := s.pool.QueryRow(ctx, `SELECT deleted_at IS NULL FROM (
		SELECT deleted_at FROM conversations.pymodules
		WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2
		ORDER BY id DESC LIMIT 1
	) latest`, ownerArg(ownerUserID), name).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return pymodules.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete pymodule: %w", err)
	}
	if !live {
		return pymodules.ErrNotFound
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO conversations.pymodules (owner_user_id, name, code, description, deleted_at)
		 VALUES ($1, $2, '', '', now())`, ownerArg(ownerUserID), name); err != nil {
		return fmt.Errorf("delete pymodule: %w", err)
	}
	return nil
}
