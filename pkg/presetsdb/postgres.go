// SPDX-License-Identifier: Apache-2.0

// Package presetsdb is the Postgres implementation of presets.Store. It is
// the only package that may touch pgx on the preset path -- see pkg/presets.
package presetsdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/presets"
)

// NewPostgresStore creates a presets.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) presets.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

// selectCols lists every conversations.presets column in presets.Record
// order. Nullable-by-absence text columns (provider, model, thinking,
// executor, system_prompt, append_system_prompt, written_by_child) are
// COALESCEd to ” because the domain type uses the empty string as its
// unset marker; tools/skills/mcp_servers are NOT coalesced -- their tri-state
// nil (kind default) vs non-nil-empty (none) is the whole point.
const selectCols = `id,
	COALESCE(owner_user_id::text, ''),
	name,
	description,
	kind,
	COALESCE(provider, ''),
	COALESCE(model, ''),
	COALESCE(thinking, ''),
	COALESCE(executor, ''),
	labels,
	tools,
	skills,
	mcp_servers,
	context_files,
	COALESCE(system_prompt, ''),
	COALESCE(append_system_prompt, ''),
	max_cost,
	max_depth,
	max_children,
	COALESCE(written_by_child, ''),
	deleted_at,
	created_at`

// scanRecord scans one row in selectCols order. A NULL jsonb labels column
// is impossible (NOT NULL DEFAULT '{}'), but a NULL-safe substitute keeps
// Record's "never nil after a read" contract even for a hand-written row.
func scanRecord(row pgx.Row) (presets.Record, error) {
	var r presets.Record
	err := row.Scan(&r.ID, &r.OwnerUserID, &r.Name, &r.Description, &r.Kind,
		&r.Provider, &r.Model, &r.Thinking, &r.Executor,
		&r.Labels, &r.Tools, &r.Skills, &r.MCPServers,
		&r.ContextFiles, &r.SystemPrompt, &r.AppendSystemPrompt,
		&r.MaxCost, &r.MaxDepth, &r.MaxChildren,
		&r.WrittenByChild, &r.DeletedAt, &r.CreatedAt)
	if err == nil && r.Labels == nil {
		r.Labels = map[string]string{}
	}
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

// Put always inserts -- nothing is ever updated and no existing row is
// touched. r.ID, r.OwnerUserID, r.CreatedAt and r.DeletedAt are ignored:
// versioning is the identity column, ownership the daemon's argument, and
// deleted_at belongs to Delete. Empty optional strings become SQL NULL via
// NULLIF so the "unset" marker round-trips. labels is passed as a non-nil
// map -- encoding a nil map would write NULL and violate NOT NULL.
func (s *pgStore) Put(ctx context.Context, ownerUserID string, r presets.Record) (presets.Record, error) {
	labels := r.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out, err := scanRecord(s.pool.QueryRow(ctx,
		`INSERT INTO conversations.presets (
			owner_user_id, name, description, kind, provider, model, thinking,
			executor, labels, tools, skills, mcp_servers, context_files,
			system_prompt, append_system_prompt, max_cost, max_depth, max_children,
			written_by_child)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''),
			NULLIF($8, ''), $9, $10, $11, $12, $13,
			NULLIF($14, ''), NULLIF($15, ''), $16, $17, $18,
			NULLIF($19, ''))
		 RETURNING `+selectCols,
		ownerArg(ownerUserID), r.Name, r.Description, r.Kind,
		r.Provider, r.Model, r.Thinking, r.Executor, labels,
		r.Tools, r.Skills, r.MCPServers, r.ContextFiles,
		r.SystemPrompt, r.AppendSystemPrompt, r.MaxCost, r.MaxDepth, r.MaxChildren,
		r.WrittenByChild))
	if err != nil {
		return presets.Record{}, fmt.Errorf("put preset: %w", err)
	}
	return out, nil
}

// List returns the latest live row per name whose name starts with prefix.
// IS NOT DISTINCT FROM (not "=") is required because SQL NULL = NULL is
// never true: without it, every unattributed caller (ownerUserID == "")
// would match ZERO rows instead of every other unattributed row. The prefix
// filter is left(name, length($2)) = $2, deliberately NOT LIKE -- a `_` in
// a prefix is a literal underscore in a preset name, not a wildcard.
func (s *pgStore) List(ctx context.Context, ownerUserID, prefix string) ([]presets.Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (name) `+selectCols+`
		 FROM conversations.presets
		 WHERE owner_user_id IS NOT DISTINCT FROM $1
			AND ($2 = '' OR left(name, length($2)) = $2)
			AND deleted_at IS NULL
		 ORDER BY name, id DESC`,
		ownerArg(ownerUserID), prefix)
	if err != nil {
		return nil, fmt.Errorf("list presets: %w", err)
	}
	defer rows.Close()
	var out []presets.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan preset: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get mirrors List's per-name rule for one name: latest live row by id,
// owner-scoped with the same IS NOT DISTINCT FROM semantics for the
// unattributed bucket. pgx.ErrNoRows becomes presets.ErrNotFound -- an
// ANSWER, not an error of the store.
func (s *pgStore) Get(ctx context.Context, ownerUserID, name string) (presets.Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.presets
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2 AND deleted_at IS NULL
		 ORDER BY id DESC
		 LIMIT 1`,
		ownerArg(ownerUserID), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return presets.Record{}, presets.ErrNotFound
	}
	if err != nil {
		return presets.Record{}, fmt.Errorf("get preset: %w", err)
	}
	return r, nil
}

// History returns every row for name, live or deleted, newest first --
// exactly the rows Get filters out -- or ErrNotFound when there are none.
// It is the audit view of the append-only store: a delete never removes a
// row, it stamps deleted_at in place.
func (s *pgStore) History(ctx context.Context, ownerUserID, name string) ([]presets.Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+`
		 FROM conversations.presets
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2
		 ORDER BY id DESC`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return nil, fmt.Errorf("history presets: %w", err)
	}
	defer rows.Close()
	var out []presets.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan preset: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history presets: %w", err)
	}
	if len(out) == 0 {
		return nil, presets.ErrNotFound
	}
	return out, nil
}

// Delete soft-deletes every live version of name by stamping deleted_at on
// all of them -- the only mutation any preset row ever undergoes; Put is
// insert-only. RowsAffected == 0 means no live row existed (unknown name or
// already deleted) -> ErrNotFound, nothing written. A Put that commits after
// this statement is a new live row and restores the name.
func (s *pgStore) Delete(ctx context.Context, ownerUserID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.presets SET deleted_at = now()
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2 AND deleted_at IS NULL`,
		ownerArg(ownerUserID), name)
	if err != nil {
		return fmt.Errorf("delete preset: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return presets.ErrNotFound
	}
	return nil
}
