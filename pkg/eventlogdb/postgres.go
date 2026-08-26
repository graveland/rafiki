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

// Append assigns the next per-child ordinal inside the INSERT, so there is no
// read-then-write window. Two concurrent appends for one child can still both
// compute the same MAX+1; the primary key then rejects the loser with 23505
// and we retry. Bounded at 16 attempts: contention here means multiple
// goroutines are publishing for one child, so retrying absorbs bursts while
// a persistent conflict is surfaced rather than looping forever.
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
		var ord int32
		err := s.pool.QueryRow(ctx, appendSQL, childID, eventlog.TypeName(ev), payload).Scan(&ord)
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
