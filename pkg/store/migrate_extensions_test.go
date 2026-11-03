// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestMigrateFullServerSchemaUnchanged pins the schema the chain produces on a
// server that has timescaledb, vector and pg_textsearch installed: plain tables
// for the three turn tables — 0001, 0009 and 0010 no longer branch on the
// extension, so a database created from the chain never sees a hypertable —
// real vector embedding columns and bm25 indexes from 0036.
//
// The plain-table half is checked at 0067, in its own scratch database, because
// 0068 converts any hypertable it finds: after the full chain the assertion
// would hold even if the baseline went back to creating hypertables. On a server
// WITHOUT timescaledb it fails, rather than passing vacuously: the catalog it
// needs is absent, so the property cannot be observed at all.
//
// 0036's unavailable branch (bytea embedding columns, no bm25 indexes) is
// exercised on a real embedded server, which has neither extension.
func TestMigrateFullServerSchemaUnchanged(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	// 0001, 0009 and 0010 create plain tables unconditionally. The check has to
	// run BEFORE 0068 exists: 0068 converts any hypertable it finds, so after the
	// full chain this assertion holds even if the baseline went back to creating
	// hypertables.
	baseline := testPool(t)
	c.Require().NoError(migrateTo(ctx, baseline, 67), "migrate to 0067")
	var hasCatalog bool
	c.Require().NoError(baseline.QueryRow(ctx,
		`SELECT to_regclass('timescaledb_information.hypertables') IS NOT NULL`).Scan(&hasCatalog),
		"probe the timescaledb catalog")
	c.True(hasCatalog, "this test needs a server with timescaledb installed in template1; without it a plain-table assertion cannot tell a regression from a missing catalog")
	for _, table := range []string{"conversation_turn", "broadcast", "raw_http_request"} {
		var isHyper bool
		c.Require().NoError(baseline.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM timescaledb_information.hypertables
				 WHERE hypertable_name = $1)`, table).Scan(&isHyper), "probe hypertable %s at 0067", table)
		c.False(isHyper, "%s is a hypertable at 0067; the baseline creates plain tables", table)
	}

	// And nothing in the chain leaves one behind either.
	for _, table := range []string{"conversation_turn", "broadcast", "raw_http_request"} {
		var isHyper bool
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM timescaledb_information.hypertables
				 WHERE hypertable_name = $1)`, table).Scan(&isHyper), "probe hypertable %s", table)
		c.False(isHyper, "%s is a hypertable after the full chain", table)
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
