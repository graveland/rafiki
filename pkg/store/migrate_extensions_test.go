// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestMigrateFullServerSchemaUnchanged pins the AVAILABLE branch of the four
// migrations that tolerate a missing extension (0001, 0009, 0010 and 0036).
// The test database has timescaledb, vector and pg_textsearch, so the chain
// must still produce exactly the schema it produced before those migrations
// learned to degrade: hypertables, vector embedding columns, bm25 indexes. A
// DO block that silently takes the "extension missing" path — a typo in the
// extname, a check against the wrong catalog — shows up here as a plain table,
// a bytea column or a missing index.
//
// The unavailable branch (plain tables, bytea embedding columns, no bm25
// indexes) is exercised on a real embedded server, which has none of the three.
func TestMigrateFullServerSchemaUnchanged(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	// 0001, 0009 and 0010: a plain table where a hypertable is expected would
	// silently lose chunk pruning and the columnstore policy those tables rely
	// on, and nothing else in the chain would notice.
	for _, table := range []string{"conversation_turn", "broadcast", "raw_http_request"} {
		var isHyper bool
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM timescaledb_information.hypertables
				 WHERE hypertable_name = $1)`, table).Scan(&isHyper), "probe hypertable %s", table)
		c.True(isHyper, "%s is a plain table on a server with timescaledb installed", table)
	}

	// 0036: the bytea placeholders must become real vector columns, or every
	// recall query that stores or compares an embedding breaks.
	for _, table := range []string{"memory", "conversation_window", "conversation_summary"} {
		var typ string
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT format_type(atttypid, atttypmod) FROM pg_attribute
			 WHERE attrelid = ('conversations.' || $1::text)::regclass
			   AND attname = 'embedding'`, table).Scan(&typ), "probe %s.embedding", table)
		c.Eq("vector", typ, "conversations.%s.embedding type", table)
	}

	// 0036: and the bm25 indexes must be created, or recall's keyword search
	// loses the access method it plans against.
	for _, index := range []string{"memory_bm25", "conversation_window_bm25", "conversation_summary_bm25"} {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				 WHERE schemaname = 'conversations' AND indexname = $1)`, index).Scan(&exists),
			"probe index %s", index)
		c.True(exists, "conversations.%s is missing on a server with pg_textsearch", index)
	}
}
