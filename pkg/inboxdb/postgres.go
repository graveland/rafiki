// SPDX-License-Identifier: Apache-2.0

// Package inboxdb is the Postgres implementation of inbox.Store.
//
// It is a separate package for the same reason tasksdb and eventlogdb are:
// Go imports whole packages, so while this lived in pkg/inbox every binary
// touching an inbox type would link pgx -- including cmd/rafiki, a socket
// client that must never open a database. The Store INTERFACE and every data
// type stay in pkg/inbox.
package inboxdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/inbox"
)

const acceptSQL = `
INSERT INTO conversations.agent_inbox (id, child_id, mode, source, coalesce_key, body)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING accepted_at`

const pendingSQL = `
SELECT id, child_id, mode, source, coalesce_key, body, accepted_at
  FROM conversations.agent_inbox
 WHERE child_id = $1 AND state = 'pending'
 ORDER BY accepted_at ASC, id ASC`

// markSentSQL refuses to move anything that is not pending. A row already
// consumed must never go back to sent: the ack has landed, the message is in
// a turn, and re-sending it would duplicate work the model has already done.
const markSentSQL = `
UPDATE conversations.agent_inbox
   SET state = 'sent', updated_at = now()
 WHERE id = ANY($1) AND state = 'pending'`

const markConsumedSQL = `
UPDATE conversations.agent_inbox
   SET state = 'consumed', updated_at = now()
 WHERE id = ANY($1) AND state IN ('pending', 'sent')`

// resetSentSQL is scoped to ONE child on purpose. See the table comment: an
// unscoped version reaches into another live daemon's in-flight messages.
const resetSentSQL = `
UPDATE conversations.agent_inbox
   SET state = 'pending', updated_at = now()
 WHERE child_id = $1 AND state = 'sent'`

const dropSQL = `
UPDATE conversations.agent_inbox
   SET state = 'dropped', drop_reason = $2, updated_at = now()
 WHERE child_id = $1 AND state IN ('pending', 'sent')`

const sweepSQL = `
DELETE FROM conversations.agent_inbox
 WHERE state IN ('consumed', 'dropped') AND accepted_at < $1`

// Store is a PostgreSQL-backed inbox.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns an inbox.Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Accept(ctx context.Context, in inbox.Inbound) (inbox.Inbound, error) {
	if in.ChildID == "" {
		return inbox.Inbound{}, errors.New("inbox: ChildID is required")
	}
	id, err := inbox.NewID()
	if err != nil {
		return inbox.Inbound{}, err
	}
	in.ID = id
	var acceptedAt time.Time
	if err := s.pool.QueryRow(ctx, acceptSQL,
		in.ID, in.ChildID, in.Mode.String(), in.Source, in.Key, in.Text,
	).Scan(&acceptedAt); err != nil {
		return inbox.Inbound{}, fmt.Errorf("inbox: accept: %w", err)
	}
	in.AcceptedAt = acceptedAt.UTC()
	return in, nil
}

func (s *Store) Pending(ctx context.Context, childID string) ([]inbox.Inbound, error) {
	rows, err := s.pool.Query(ctx, pendingSQL, childID)
	if err != nil {
		return nil, fmt.Errorf("inbox: pending: %w", err)
	}
	defer rows.Close()

	var out []inbox.Inbound
	for rows.Next() {
		var r inbox.Inbound
		var mode string
		if err := rows.Scan(&r.ID, &r.ChildID, &mode, &r.Source, &r.Key, &r.Text, &r.AcceptedAt); err != nil {
			return nil, fmt.Errorf("inbox: scan: %w", err)
		}
		r.Mode = inbox.ParseMode(mode)
		r.AcceptedAt = r.AcceptedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inbox: iterate: %w", err)
	}
	return out, nil
}

func (s *Store) MarkSent(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, markSentSQL, ids); err != nil {
		return fmt.Errorf("inbox: mark sent: %w", err)
	}
	return nil
}

func (s *Store) MarkConsumed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, markConsumedSQL, ids); err != nil {
		return fmt.Errorf("inbox: mark consumed: %w", err)
	}
	return nil
}

func (s *Store) ResetSent(ctx context.Context, childID string) (int, error) {
	if childID == "" {
		return 0, errors.New("inbox: ResetSent requires a child id")
	}
	tag, err := s.pool.Exec(ctx, resetSentSQL, childID)
	if err != nil {
		return 0, fmt.Errorf("inbox: reset sent: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) Drop(ctx context.Context, childID, reason string) (int, error) {
	if childID == "" {
		return 0, errors.New("inbox: Drop requires a child id")
	}
	tag, err := s.pool.Exec(ctx, dropSQL, childID, reason)
	if err != nil {
		return 0, fmt.Errorf("inbox: drop: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) Sweep(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, sweepSQL, before)
	if err != nil {
		return 0, fmt.Errorf("inbox: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

var _ inbox.Store = (*Store)(nil)
