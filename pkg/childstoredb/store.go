// SPDX-License-Identifier: Apache-2.0

// Package childstoredb is the Postgres implementation of
// childstore.ChildStore. It owns every SQL statement against
// conversations.child, keeping the controller free of them.
package childstoredb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
)

// Store persists child records to conversations.child.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ childstore.ChildStore = (*Store)(nil)

const upsertSQL = `
INSERT INTO conversations.child
    (child_id, conversation_id, owner_user_id, kind, name, cwd, config_dir,
     pid, daemon_id, ns_token, provider, model, thinking, session_file, session_dir,
     session_id, no_session, status, last_status, spawned_at, last_activity,
     exited_at, exit_code, exit_signal, executor_selector, workspace_mode,
     max_depth, max_cost, max_children, config, labels)
VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6, $7,
        $8, $9, $10, $11, $12, $13, $14, $15,
        $16, $17, $18, $19, $20, $21,
        $22, $23, $24, $25, $26,
        $27, $28, $29, $30, $31)
ON CONFLICT (child_id) DO UPDATE SET
    status            = EXCLUDED.status,
    last_status       = COALESCE(EXCLUDED.last_status, conversations.child.last_status),
    conversation_id   = COALESCE(EXCLUDED.conversation_id, conversations.child.conversation_id),
    owner_user_id     = COALESCE(EXCLUDED.owner_user_id, conversations.child.owner_user_id),
    name              = EXCLUDED.name,
    cwd               = EXCLUDED.cwd,
    config_dir        = EXCLUDED.config_dir,
    pid               = EXCLUDED.pid,
    daemon_id         = EXCLUDED.daemon_id,
    ns_token          = EXCLUDED.ns_token,
    provider          = EXCLUDED.provider,
    model             = EXCLUDED.model,
    thinking          = EXCLUDED.thinking,
    session_file      = EXCLUDED.session_file,
    session_dir       = EXCLUDED.session_dir,
    session_id        = EXCLUDED.session_id,
    no_session        = EXCLUDED.no_session,
    last_activity     = EXCLUDED.last_activity,
    exited_at         = EXCLUDED.exited_at,
    exit_code         = EXCLUDED.exit_code,
    exit_signal       = EXCLUDED.exit_signal,
    executor_selector = EXCLUDED.executor_selector,
    workspace_mode    = EXCLUDED.workspace_mode,
    max_depth         = EXCLUDED.max_depth,
    max_cost          = EXCLUDED.max_cost,
    max_children      = EXCLUDED.max_children,
    config            = EXCLUDED.config,
    labels            = EXCLUDED.labels,
    updated_at        = now()`

// Upsert writes rec, creating the row or updating it in place.
//
// The two COALESCEs are load-bearing. last_status is written once, by the exit
// path, and an ordinary status write must not blank it or the recovery
// predicate loses the only value it reads. conversation_id becomes known after
// the row already exists, and a later write that has not re-read it must not
// erase the correlation.
func (s *Store) Upsert(ctx context.Context, rec childstore.ChildRecord) error {
	config, err := json.Marshal(rec.Config)
	if err != nil {
		return fmt.Errorf("childstoredb: marshal config: %w", err)
	}
	labels := rec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("childstoredb: marshal labels: %w", err)
	}

	_, err = s.pool.Exec(ctx, upsertSQL,
		rec.ChildID, nullString(rec.ConversationID), nullString(rec.OwnerUserID),
		rec.Kind, rec.Name, rec.Cwd, rec.ConfigDir,
		nullInt(rec.PID), rec.DaemonID, rec.NSToken, rec.Provider, rec.Model, rec.Thinking,
		rec.SessionFile, rec.SessionDir, rec.SessionID, rec.NoSession,
		rec.Status, nullString(rec.LastStatus),
		rec.SpawnedAt, nullTime(rec.LastActivity), nullTime(rec.ExitedAt),
		rec.ExitCode, rec.ExitSignal,
		rec.ExecutorSelector, rec.WorkspaceMode,
		rec.MaxDepth, rec.MaxCost, rec.MaxChildren,
		config, labelsJSON)
	if err != nil {
		return fmt.Errorf("childstoredb: upsert %s: %w", rec.ChildID, err)
	}
	return nil
}

// Delete removes a child row. Idempotent: a missing row is not an error.
func (s *Store) Delete(ctx context.Context, childID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM conversations.child WHERE child_id = $1`, childID); err != nil {
		return fmt.Errorf("childstoredb: delete %s: %w", childID, err)
	}
	return nil
}

const listSQL = `
SELECT child_id, COALESCE(conversation_id::text, ''), COALESCE(owner_user_id::text, ''),
       kind, COALESCE(name,''), COALESCE(cwd,''), COALESCE(config_dir,''),
       COALESCE(pid, 0), COALESCE(daemon_id,''), COALESCE(ns_token,''),
       COALESCE(provider,''), COALESCE(model,''), COALESCE(thinking,''),
       COALESCE(session_file,''), COALESCE(session_dir,''), COALESCE(session_id,''),
       no_session, status, COALESCE(last_status,''),
       spawned_at, last_activity, exited_at, exit_code, COALESCE(exit_signal,''),
       COALESCE(executor_selector,''), COALESCE(workspace_mode,''),
       max_depth, max_cost, max_children, config, labels, updated_at
  FROM conversations.child`

// List returns every child row.
//
// It returns all of them rather than filtering: the caller loads every row into
// the in-memory store (so rafiki list shows the same set it always did) and
// applies the recovery predicate itself.
func (s *Store) List(ctx context.Context) ([]childstore.ChildRecord, error) {
	rows, err := s.pool.Query(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("childstoredb: list: %w", err)
	}
	defer rows.Close()

	var out []childstore.ChildRecord
	for rows.Next() {
		var (
			rec          childstore.ChildRecord
			lastActivity *time.Time
			exitedAt     *time.Time
			configJSON   []byte
			labelsJSON   []byte
		)
		if err := rows.Scan(
			&rec.ChildID, &rec.ConversationID, &rec.OwnerUserID,
			&rec.Kind, &rec.Name, &rec.Cwd, &rec.ConfigDir,
			&rec.PID, &rec.DaemonID, &rec.NSToken,
			&rec.Provider, &rec.Model, &rec.Thinking,
			&rec.SessionFile, &rec.SessionDir, &rec.SessionID,
			&rec.NoSession, &rec.Status, &rec.LastStatus,
			&rec.SpawnedAt, &lastActivity, &exitedAt, &rec.ExitCode, &rec.ExitSignal,
			&rec.ExecutorSelector, &rec.WorkspaceMode,
			&rec.MaxDepth, &rec.MaxCost, &rec.MaxChildren,
			&configJSON, &labelsJSON, &rec.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("childstoredb: scan: %w", err)
		}
		if lastActivity != nil {
			rec.LastActivity = *lastActivity
		}
		if exitedAt != nil {
			rec.ExitedAt = *exitedAt
		}
		if err := json.Unmarshal(configJSON, &rec.Config); err != nil {
			return nil, fmt.Errorf("childstoredb: decode config for %s: %w", rec.ChildID, err)
		}
		if err := json.Unmarshal(labelsJSON, &rec.Labels); err != nil {
			return nil, fmt.Errorf("childstoredb: decode labels for %s: %w", rec.ChildID, err)
		}
		if len(rec.Labels) == 0 {
			rec.Labels = nil
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// nullString maps "" to SQL NULL, so an unknown uuid column stays NULL rather
// than failing a cast on the empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
