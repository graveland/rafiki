// SPDX-License-Identifier: Apache-2.0

// Package skillsdb is the Postgres implementation of skills.Store. It is the
// only package that may touch pgx on the skills path — see pkg/skills.
package skillsdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/skills"
)

// NewPostgresStore creates a skills.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) skills.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

const selectCols = `id::text, namespace, name, description, body, source,
	COALESCE(owner_user_id::text, ''), COALESCE(shadowed_core_version, ''),
	enabled, created_at, updated_at`

func scanRecord(row pgx.Row) (skills.Record, error) {
	var r skills.Record
	err := row.Scan(&r.ID, &r.Namespace, &r.Name, &r.Description, &r.Body,
		&r.Source, &r.OwnerUserID, &r.ShadowedCoreVersion, &r.Enabled,
		&r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s *pgStore) List(ctx context.Context, enabledOnly bool) ([]skills.Record, error) {
	q := `SELECT ` + selectCols + ` FROM conversations.skills`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY namespace, name`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	var out []skills.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgStore) Get(ctx context.Context, namespace, name string) (skills.Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+` FROM conversations.skills
		 WHERE namespace = $1 AND name = $2`, namespace, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Record{}, skills.ErrNotFound
	}
	if err != nil {
		return skills.Record{}, fmt.Errorf("get skill: %w", err)
	}
	return r, nil
}

// Upsert deliberately omits `enabled` from the DO UPDATE SET list: re-syncing
// content must never re-enable a row an operator disabled. The insert path
// still honours r.Enabled for a brand-new row.
//
// A disabled row is invisible to the partial unique index below — it carries
// only enabled rows — so the INSERT cannot conflict with one and would
// silently create a second, enabled row under the same name. The disabled
// case is therefore handled first: re-upserting the SAME source over a
// disabled row is content staying fresh, so the row is updated in place and
// stays disabled. A different source is a new claim on the freed name and
// falls through to the INSERT, which is how an operator override reclaims a
// disabled core skill.
func (s *pgStore) Upsert(ctx context.Context, r skills.Record) (skills.Record, error) {
	if r.Namespace == "" {
		r.Namespace = skills.DefaultNamespace
	}
	var ownerArg, shadowArg any
	if r.OwnerUserID != "" {
		ownerArg = r.OwnerUserID
	}
	if r.ShadowedCoreVersion != "" {
		shadowArg = r.ShadowedCoreVersion
	}
	refreshed, err := scanRecord(s.pool.QueryRow(ctx,
		`UPDATE conversations.skills SET
		   description = $3, body = $4, source = $5,
		   owner_user_id = $6, shadowed_core_version = $7, updated_at = now()
		 WHERE namespace = $1 AND name = $2 AND source = $5 AND NOT enabled
		 RETURNING `+selectCols,
		r.Namespace, r.Name, r.Description, r.Body, r.Source, ownerArg, shadowArg))
	if err == nil {
		return refreshed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return skills.Record{}, fmt.Errorf("upsert skill: %w", err)
	}
	out, err := scanRecord(s.pool.QueryRow(ctx,
		`INSERT INTO conversations.skills
		   (namespace, name, description, body, source, owner_user_id,
		    shadowed_core_version, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (namespace, name) WHERE enabled DO UPDATE SET
		   description = EXCLUDED.description,
		   body = EXCLUDED.body,
		   source = EXCLUDED.source,
		   owner_user_id = EXCLUDED.owner_user_id,
		   shadowed_core_version = EXCLUDED.shadowed_core_version,
		   updated_at = now()
		 RETURNING `+selectCols,
		r.Namespace, r.Name, r.Description, r.Body, r.Source,
		ownerArg, shadowArg, r.Enabled))
	if err != nil {
		return skills.Record{}, fmt.Errorf("upsert skill: %w", err)
	}
	return out, nil
}

// In the override state one name carries a disabled row and an enabled one,
// so the update scopes itself to the rows it is actually flipping: enable
// touches only disabled rows (AND NOT enabled), disable only enabled ones
// (AND enabled) — the single `enabled <> $3` predicate is both. Flipping a
// row already in the target state matches nothing and surfaces as
// ErrNotFound, like an absent name.
func (s *pgStore) SetEnabled(ctx context.Context, namespace, name string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.skills SET enabled = $3, updated_at = now()
		 WHERE namespace = $1 AND name = $2 AND enabled <> $3`, namespace, name, enabled)
	if err != nil {
		return fmt.Errorf("set skill enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return skills.ErrNotFound
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, namespace, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM conversations.skills WHERE namespace = $1 AND name = $2`,
		namespace, name)
	if err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return skills.ErrNotFound
	}
	return nil
}

// ReplaceNamespaceSource runs in ONE transaction: a partial apply would leave
// the corpus in a state no version of rafiki ever published.
func (s *pgStore) ReplaceNamespaceSource(ctx context.Context, namespace, source string, want []skills.Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	keep := make([]string, 0, len(want))
	for _, r := range want {
		keep = append(keep, r.Name)
		var shadowArg any
		if r.ShadowedCoreVersion != "" {
			shadowArg = r.ShadowedCoreVersion
		}
		// A disabled row of the same source is refreshed in place instead of
		// being re-inserted: the INSERT below cannot see it (the partial index
		// carries only enabled rows), so reaching the INSERT would resurrect
		// the very skill an operator switched off.
		tag, err := tx.Exec(ctx,
			`UPDATE conversations.skills
			   SET description = $3, body = $4, shadowed_core_version = $6, updated_at = now()
			 WHERE namespace = $1 AND name = $2 AND source = $5 AND NOT enabled`,
			namespace, r.Name, r.Description, r.Body, source, shadowArg)
		if err != nil {
			return fmt.Errorf("refresh disabled %s:%s: %w", namespace, r.Name, err)
		}
		if tag.RowsAffected() > 0 {
			continue
		}
		// The WHERE on DO UPDATE skips a conflicting row of a different source:
		// an enabled operator override must not have the sync's body written
		// over it.
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversations.skills
			   (namespace, name, description, body, source, shadowed_core_version, enabled)
			 VALUES ($1,$2,$3,$4,$5,$6,TRUE)
			 ON CONFLICT (namespace, name) WHERE enabled DO UPDATE SET
			   description = EXCLUDED.description,
			   body = EXCLUDED.body,
			   shadowed_core_version = EXCLUDED.shadowed_core_version,
			   updated_at = now()
			 WHERE conversations.skills.source = EXCLUDED.source`,
			namespace, r.Name, r.Description, r.Body, source, shadowArg); err != nil {
			return fmt.Errorf("upsert %s:%s: %w", namespace, r.Name, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM conversations.skills
		 WHERE namespace = $1 AND source = $2 AND NOT (name = ANY($3))`,
		namespace, source, keep); err != nil {
		return fmt.Errorf("prune %s/%s: %w", namespace, source, err)
	}
	return tx.Commit(ctx)
}
