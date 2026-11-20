// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestMigrate0075BackfillsExtractedOrdinalFromTheLastWindow pins 0075: a
// conversation with windows takes its LAST window's end (the value the previous
// pending test compared against, so nothing is treated as caught up that was
// not), and one without windows stays NULL.
func TestMigrate0075BackfillsExtractedOrdinalFromTheLastWindow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 74), "migrate to 0074, one short of 0075")

	newConversation := func() string {
		t.Helper()
		var id string
		c.Require().NoError(pool.QueryRow(ctx, `
			INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
			VALUES ('test', 'test') RETURNING id::text`).Scan(&id), "insert conversation")
		return id
	}
	window := func(convID string, seq, from, to int) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_window
				(conversation_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
			VALUES ($1::uuid, $2, $3, $4, 'text', $5, 1)`, convID, seq, from, to, seq == 0)
		c.Require().NoError(err, "insert window %d", seq)
	}

	windowed := newConversation()
	window(windowed, 0, 0, 4)
	window(windowed, 1, 4, 9)
	bare := newConversation()

	c.Require().NoError(migrateTo(ctx, pool, 75), "migrate to 0075")

	extracted := func(id string) *int {
		t.Helper()
		var v *int
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT extracted_ordinal FROM conversations.conversation WHERE id = $1::uuid`, id).Scan(&v),
			"read extracted_ordinal")
		return v
	}
	got := extracted(windowed)
	c.Require().True(got != nil, "a conversation with windows is backfilled")
	c.Eq(9, *got, "the last window's end")
	c.True(extracted(bare) == nil, "a conversation with no windows stays NULL")
}
