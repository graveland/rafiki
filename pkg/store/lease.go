// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lease is one daemon's exclusive right to WRITE to a conversation.
//
// Reads are never gated — every daemon may read every conversation, which is
// what makes search across several machines work. Only appends are exclusive.
type Lease struct {
	ConversationID string
	Holder         string
	Token          string
	ExpiresAt      time.Time
}

// Held reports whether l names an actual lease rather than the zero value.
// An unfenced caller (the proxy path, a client-driven conversation) carries the
// zero Lease, and every guard treats that as "not fenced".
func (l Lease) Held() bool { return l.ConversationID != "" && l.Token != "" }

// LeaseStore owns conversations.conversation_lease.
type LeaseStore struct {
	pool *pgxpool.Pool
}

// NewLeases returns a LeaseStore backed by pool.
func NewLeases(pool *pgxpool.Pool) *LeaseStore { return &LeaseStore{pool: pool} }

const acquireSQL = `
INSERT INTO conversations.conversation_lease (conversation_id, holder, token, expires_at)
VALUES ($1::uuid, $2, gen_random_uuid(), now() + $3::interval)
ON CONFLICT (conversation_id) DO UPDATE
   SET holder      = EXCLUDED.holder,
       token       = EXCLUDED.token,
       acquired_at = now(),
       expires_at  = EXCLUDED.expires_at
 WHERE conversations.conversation_lease.expires_at < now()
    OR conversations.conversation_lease.holder = EXCLUDED.holder
RETURNING token::text, expires_at`

// Acquire takes the write lease on a conversation.
//
// ok=false means a different daemon holds a live lease. Back off; do not retry
// in a loop.
//
// The "OR holder = EXCLUDED.holder" clause is what lets a restarted daemon
// reclaim its own leases instantly, independent of TTL — the TTL only ever
// gates takeover by a DIFFERENT holder. It is also exactly why two daemons
// sharing one id reproduce split-brain: each reclaims the other's leases on
// every acquire.
func (s *LeaseStore) Acquire(ctx context.Context, conversationID, holder string, ttl time.Duration) (Lease, bool, error) {
	var token string
	var expires time.Time
	err := s.pool.QueryRow(ctx, acquireSQL, conversationID, holder, intervalOf(ttl)).Scan(&token, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("lease: acquire %s: %w", conversationID, err)
	}
	return Lease{ConversationID: conversationID, Holder: holder, Token: token, ExpiresAt: expires}, true, nil
}

// Renew extends a held lease. ok=false means the lease is gone — taken over,
// released, or the conversation deleted — and the caller must stop writing.
func (s *LeaseStore) Renew(ctx context.Context, l Lease, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE conversations.conversation_lease
		   SET expires_at = now() + $3::interval
		 WHERE conversation_id = $1::uuid AND token = $2::uuid`,
		l.ConversationID, l.Token, intervalOf(ttl))
	if err != nil {
		return false, fmt.Errorf("lease: renew %s: %w", l.ConversationID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Release drops a lease on clean shutdown. Idempotent.
func (s *LeaseStore) Release(ctx context.Context, l Lease) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid AND token = $2::uuid`,
		l.ConversationID, l.Token); err != nil {
		return fmt.Errorf("lease: release %s: %w", l.ConversationID, err)
	}
	return nil
}

// Valid reports whether l is still the live lease. Used on the zero-rows path
// of a guarded write to tell "lease lost" from an ordinary conflict.
func (s *LeaseStore) Valid(ctx context.Context, l Lease) (bool, error) {
	if !l.Held() {
		return false, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid AND holder = $2 AND token = $3::uuid
		   AND expires_at > now()`,
		l.ConversationID, l.Holder, l.Token).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("lease: validate %s: %w", l.ConversationID, err)
	}
	return n == 1, nil
}

// intervalOf renders a duration as a Postgres interval literal. Seconds rather
// than the Go string form, which Postgres does not parse.
func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d/time.Second))
}
