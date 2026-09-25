// SPDX-License-Identifier: Apache-2.0

package eventlogdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

const defaultReadLimit = 1000

const appendSQL = `
INSERT INTO conversations.event_log (child_id, ordinal, type, payload)
SELECT $1, COALESCE(MAX(ordinal) + 1, 0), $2, $3
  FROM conversations.event_log WHERE child_id = $1
RETURNING ordinal`

// appendLockSQL takes the per-child append lock. The key is namespaced with a
// 'rafiki.event_log:' prefix so its hash can never collide with another
// advisory-lock user in this schema (pkg/store/migrate.go locks a fixed int64,
// pkg/tasksdb keys 'conv|parent' partitions, pkg/recalldb locks
// 'rafiki.recall.indexer'). Transaction-scoped, so it releases on commit or
// rollback with no explicit unlock.
const appendLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('rafiki.event_log:' || $1, 0))`

const readSQL = `
SELECT child_id, ordinal, type, payload, created_at
  FROM conversations.event_log
 WHERE child_id = $1 AND ordinal > $2
 ORDER BY ordinal ASC
 LIMIT $3`

const latestSQL = `
SELECT MAX(ordinal)
  FROM conversations.event_log
 WHERE child_id = $1`

// Store is a PostgreSQL-backed eventlog.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns an eventlog.Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const maxAppendAttempts = 16

// Append assigns the next per-child ordinal to ev and returns it.
//
// The ordinal is computed inside the INSERT (MAX + 1), so there is no
// read-then-write window, but two concurrent appends for one child can still
// both compute the same MAX+1. To make that impossible the append runs in a
// transaction that first takes pg_advisory_xact_lock keyed on the child
// (namespaced 'rafiki.event_log:'), so only one publisher for a child ever
// reads MAX(ordinal) at a time and the lock releases on commit or rollback.
// Without that lock, several goroutines publishing for one child kept
// colliding and exhausting the retry budget, and the losing events were
// dropped entirely (controller's agent_status publisher hit this in
// production). The bounded 23505 retry stays as a safety net for a writer
// that bypasses this path; with the lock it should never trigger.
func (s *Store) Append(ctx context.Context, childID string, ev *rafikiv1.Event) (int32, error) {
	if eventlog.TierOf(ev) != eventlog.TierDurable {
		return 0, fmt.Errorf("eventlog: refusing to append ephemeral event %q", eventlog.TypeName(ev))
	}
	if childID == "" {
		return 0, errors.New("eventlog: empty child id")
	}
	payload, err := protojson.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("eventlog: marshal: %w", err)
	}
	var lastErr error
	for range maxAppendAttempts {
		ord, err := s.appendOnce(ctx, childID, eventlog.TypeName(ev), payload)
		if err == nil {
			return ord, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			lastErr = err
			continue
		}
		return 0, fmt.Errorf("eventlog: append: %w", err)
	}
	return 0, fmt.Errorf("eventlog: append: contention after %d attempts: %w", maxAppendAttempts, lastErr)
}

// appendOnce performs one locked insert attempt: begin, take the per-child
// lock, run the INSERT ... MAX+1 ... RETURNING under it, commit. The
// transaction-scoped lock releases on commit or on the deferred rollback of
// any error path, so a failed attempt never wedges the child's lock.
func (s *Store) appendOnce(ctx context.Context, childID, typeName string, payload []byte) (int32, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("eventlog: append: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize on the child BEFORE the MAX(ordinal) read; taking the lock
	// after the read would leave the read-then-write window open.
	if _, err := tx.Exec(ctx, appendLockSQL, childID); err != nil {
		return 0, fmt.Errorf("eventlog: append: lock: %w", err)
	}
	var ord int32
	if err := tx.QueryRow(ctx, appendSQL, childID, typeName, payload).Scan(&ord); err != nil {
		return 0, fmt.Errorf("eventlog: append: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("eventlog: append: commit: %w", err)
	}
	return ord, nil
}

// Read returns records for childID with Ordinal > afterOrdinal in ascending order.
func (s *Store) Read(ctx context.Context, childID string, afterOrdinal int32, limit int) ([]eventlog.Record, error) {
	if limit <= 0 {
		limit = defaultReadLimit
	}
	rows, err := s.pool.Query(ctx, readSQL, childID, afterOrdinal, limit)
	if err != nil {
		return nil, fmt.Errorf("eventlog: read: %w", err)
	}
	defer rows.Close()

	var out []eventlog.Record
	for rows.Next() {
		var r eventlog.Record
		if err := rows.Scan(&r.ChildID, &r.Ordinal, &r.Type, &r.Payload, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("eventlog: scan: %w", err)
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: iterate: %w", err)
	}
	return out, nil
}

// Latest returns the highest ordinal for childID, or ErrNotFound if no events exist.
func (s *Store) Latest(ctx context.Context, childID string) (int32, error) {
	var maxOrd *int32
	err := s.pool.QueryRow(ctx, latestSQL, childID).Scan(&maxOrd)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, eventlog.ErrNotFound
		}
		return 0, fmt.Errorf("eventlog: latest: %w", err)
	}
	if maxOrd == nil {
		return 0, eventlog.ErrNotFound
	}
	return *maxOrd, nil
}
