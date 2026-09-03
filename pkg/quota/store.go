// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns conversations.anthropic_rate_limit_status: one row per user,
// holding only the latest snapshot. A nil *Store is valid and every method is
// a safe no-op on it, matching rawtrace.RawTraceStore's pattern -- callers
// need not branch on whether quota capture is configured.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store backed by pool. Returns nil when pool is nil so
// callers can unconditionally call NewStore(pool) and get a nil-safe result.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{pool: pool}
}

// Upsert records userID's latest snapshot. No-op when s is nil.
func (s *Store) Upsert(ctx context.Context, userID string, st Status) error {
	if s == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations.anthropic_rate_limit_status
			(user_id, organization_id,
			 five_h_utilization, five_h_reset_at, five_h_status,
			 seven_d_utilization, seven_d_reset_at, seven_d_status,
			 overall_status, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (user_id) DO UPDATE SET
			organization_id     = EXCLUDED.organization_id,
			five_h_utilization  = EXCLUDED.five_h_utilization,
			five_h_reset_at     = EXCLUDED.five_h_reset_at,
			five_h_status       = EXCLUDED.five_h_status,
			seven_d_utilization = EXCLUDED.seven_d_utilization,
			seven_d_reset_at    = EXCLUDED.seven_d_reset_at,
			seven_d_status      = EXCLUDED.seven_d_status,
			overall_status      = EXCLUDED.overall_status,
			updated_at          = now()`,
		userID, nilString(st.OrganizationID),
		st.FiveH.Utilization, st.FiveH.ResetAt, nilString(st.FiveH.Status),
		st.SevenD.Utilization, st.SevenD.ResetAt, nilString(st.SevenD.Status),
		nilString(st.OverallStatus))
	if err != nil {
		return fmt.Errorf("quota: upsert %s: %w", userID, err)
	}
	return nil
}

// Get returns userID's latest snapshot. ok=false means no passthrough
// response has ever been captured for this user -- the caller's gate for
// "nothing to show", not an error.
func (s *Store) Get(ctx context.Context, userID string) (Status, bool, error) {
	if s == nil {
		return Status{}, false, nil
	}
	var st Status
	var org, fiveStatus, sevenStatus, overall *string
	var updatedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT organization_id,
		       five_h_utilization, five_h_reset_at, five_h_status,
		       seven_d_utilization, seven_d_reset_at, seven_d_status,
		       overall_status, updated_at
		  FROM conversations.anthropic_rate_limit_status
		 WHERE user_id = $1::uuid`, userID).Scan(
		&org,
		&st.FiveH.Utilization, &st.FiveH.ResetAt, &fiveStatus,
		&st.SevenD.Utilization, &st.SevenD.ResetAt, &sevenStatus,
		&overall, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("quota: get %s: %w", userID, err)
	}
	st.OrganizationID = strOrEmpty(org)
	st.FiveH.Status = strOrEmpty(fiveStatus)
	st.SevenD.Status = strOrEmpty(sevenStatus)
	st.OverallStatus = strOrEmpty(overall)
	st.UpdatedAt = updatedAt
	return st, true, nil
}

func nilString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
