// SPDX-License-Identifier: Apache-2.0

// Package modelaliasdb is the Postgres implementation of modelalias.Store. It
// is the only package that may touch pgx on the alias path -- see
// pkg/modelalias.
package modelaliasdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/modelalias"
)

// NewPostgresStore creates a modelalias.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) modelalias.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

// selectCols lists every conversations.model_alias column in modelalias.Alias
// order.
const selectCols = `name, target, created_at, updated_at`

// ownerArg turns an empty ownerUserID into a real SQL NULL rather than an
// empty-string value -- required for IS NOT DISTINCT FROM below to treat every
// unattributed row as belonging to one shared "no owner" bucket.
func ownerArg(ownerUserID string) any {
	if ownerUserID == "" {
		return nil
	}
	return ownerUserID
}

// Get is owner-scoped with IS NOT DISTINCT FROM (not "=") because SQL NULL =
// NULL is never true: without it every unattributed caller would match zero
// rows. pgx.ErrNoRows becomes modelalias.ErrNotFound -- an ANSWER, not an
// error of the store.
func (s *pgStore) Get(ctx context.Context, ownerUserID, name string) (modelalias.Alias, error) {
	var a modelalias.Alias
	err := s.pool.QueryRow(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.model_alias
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name).Scan(&a.Name, &a.Target, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelalias.Alias{}, modelalias.ErrNotFound
	}
	if err != nil {
		return modelalias.Alias{}, fmt.Errorf("modelaliasdb: get: %w", err)
	}
	return a, nil
}

// List returns every alias of the owner ordered by name; never nil.
func (s *pgStore) List(ctx context.Context, ownerUserID string) ([]modelalias.Alias, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.model_alias
		 WHERE owner_user_id IS NOT DISTINCT FROM $1
		 ORDER BY name`,
		ownerArg(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("modelaliasdb: list: %w", err)
	}
	defer rows.Close()
	out := make([]modelalias.Alias, 0)
	for rows.Next() {
		var a modelalias.Alias
		if err := rows.Scan(&a.Name, &a.Target, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("modelaliasdb: list: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("modelaliasdb: list: %w", err)
	}
	return out, nil
}

// Put validates first, then creates or replaces name's alias in one statement:
// a replace keeps created_at and moves updated_at. A replace exists so an
// alias has exactly one value -- no history is kept, by design.
func (s *pgStore) Put(ctx context.Context, ownerUserID, name, target string) (modelalias.Alias, error) {
	if err := modelalias.Validate(name, target); err != nil {
		return modelalias.Alias{}, err
	}
	var a modelalias.Alias
	err := s.pool.QueryRow(ctx,
		`INSERT INTO conversations.model_alias (owner_user_id, name, target)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (owner_user_id, name)
		 DO UPDATE SET target = EXCLUDED.target, updated_at = now()
		 RETURNING `+selectCols,
		ownerArg(ownerUserID), name, target).Scan(&a.Name, &a.Target, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return modelalias.Alias{}, fmt.Errorf("modelaliasdb: put: %w", err)
	}
	return a, nil
}

// Delete removes name's alias. RowsAffected == 0 means no such row existed ->
// ErrNotFound, nothing written.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM conversations.model_alias
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return fmt.Errorf("modelaliasdb: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return modelalias.ErrNotFound
	}
	return nil
}
