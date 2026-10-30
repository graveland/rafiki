// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// fenceLease refuses the write when lease is no longer live for THIS
// conversation: a lease held on another conversation proves nothing about this
// one. FOR SHARE on the lease row is what holds a takeover's UPDATE out for the
// rest of the transaction.
func fenceLease(ctx context.Context, tx pgx.Tx, conversationID string, lease Lease) error {
	if !lease.Held() {
		return nil
	}
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid AND holder = $2 AND token = $3::uuid
		   AND expires_at > now() FOR SHARE`, conversationID, lease.Holder, lease.Token).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lease check: %w", err)
	}
	return nil
}

// nullString renders s as a NULL for an empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// boolInt renders b as the 0/1 an integer counter adds.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
