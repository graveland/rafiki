// SPDX-License-Identifier: Apache-2.0

package eventlogdb

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const readSQL = `
SELECT child_id, ordinal, type, payload, created_at
  FROM conversations.event_log
 WHERE child_id = $1 AND ordinal > $2
 ORDER BY ordinal ASC
 LIMIT $3`

// childrenSinceSQL rides the event_log_created_at index: the range is the
// records since the bound, not the whole log.
const childrenSinceSQL = `
SELECT child_id, MIN(ordinal)
  FROM conversations.event_log
 WHERE created_at >= $1
 GROUP BY child_id`

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
// The ordinal is computed inside the INSERT (MAX + 1) and the append is that
// single statement: one round trip and one commit, with no explicit
// transaction. Two concurrent appends for one child can both compute the same
// MAX+1; (child_id, ordinal) is unique, so the loser gets a 23505 and retries.
//
// A collision costs one more statement, and the window is the length of one
// INSERT, so even a wide pool of publishers on one child resolves within the
// retry budget (TestAppendStormOnAWidePool). The append used to be a
// transaction holding a per-child advisory lock across a read and a write;
// that window was long enough for a storm to exhaust the budget and drop
// events, and the lock cost two more round trips per event.
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

// appendOnce performs one insert attempt: a single statement, atomic on its own.
func (s *Store) appendOnce(ctx context.Context, childID, typeName string, payload []byte) (int32, error) {
	var ord int32
	if err := s.pool.QueryRow(ctx, appendSQL, childID, typeName, payload).Scan(&ord); err != nil {
		return 0, err
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

// ChildrenSince maps each child with a record created at or after since to the
// lowest such ordinal.
func (s *Store) ChildrenSince(ctx context.Context, since time.Time) (map[string]int32, error) {
	rows, err := s.pool.Query(ctx, childrenSinceSQL, since)
	if err != nil {
		return nil, fmt.Errorf("eventlog: children since: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int32)
	for rows.Next() {
		var (
			childID string
			first   int32
		)
		if err := rows.Scan(&childID, &first); err != nil {
			return nil, fmt.Errorf("eventlog: children since: scan: %w", err)
		}
		out[childID] = first
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: children since: %w", err)
	}
	return out, nil
}
