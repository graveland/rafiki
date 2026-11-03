// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// hypertable0068 is one of the three tables 0068 converts, with the columnstore
// settings prod carried for it.
type hypertable0068 struct {
	table string
	// segmentBy and the 7-day compression policy are what the baseline's
	// tsdb.hypertable clause set up on prod.
	segmentBy         string
	compressionPolicy bool
}

var hypertables0068 = []hypertable0068{
	{"conversations.conversation_turn", "conversation_id", false},
	{"openrouter.broadcast", "session_id", true},
	{"conversations.raw_http_request", "source", true},
}

// makeHypertables turns the three tables into the hypertables prod runs, on top
// of the plain tables the chain now creates. Run it after migrateTo(67).
func makeHypertables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	for _, ht := range hypertables0068 {
		_, err := pool.Exec(ctx, fmt.Sprintf(
			`SELECT create_hypertable('%s', 'created_at', migrate_data => true)`, ht.table))
		c.NoError(err, "create_hypertable(%s)", ht.table)
		_, err = pool.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE %s SET (timescaledb.enable_columnstore = true, timescaledb.segmentby = '%s', timescaledb.orderby = 'created_at DESC')`,
			ht.table, ht.segmentBy))
		c.NoError(err, "columnstore settings on %s", ht.table)
		if ht.compressionPolicy {
			_, err = pool.Exec(ctx, fmt.Sprintf(
				`CALL add_columnstore_policy('%s', after => interval '7 days')`, ht.table))
			c.NoError(err, "add_columnstore_policy(%s)", ht.table)
		}
	}
}

// fixtureColumns are the columns each fixture insert supplies. Compared against
// what the catalog says an insert must supply, so a column added to one of these
// tables without a default fails here instead of as a NOT NULL violation.
var fixtureColumns = map[string][]string{
	"conversations.conversation_turn": {"conversation_id", "ordinal", "response", "request_head_id", "author_user_id", "created_at"},
	"openrouter.broadcast":            {"session_id", "model", "created_at", "raw_payload"},
	"conversations.raw_http_request":  {"source", "req_method", "req_path", "created_at"},
}

// requiredColumns reads the columns an insert into table must supply: NOT NULL,
// no default, not an identity column.
func requiredColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		 WHERE table_schema = split_part($1, '.', 1)
		   AND table_name = split_part($1, '.', 2)
		   AND is_nullable = 'NO'
		   AND column_default IS NULL
		   AND is_identity = 'NO'
		 ORDER BY ordinal_position`, table)
	c.NoError(err, "read the required columns of %s", table)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		c.NoError(rows.Scan(&name), "scan a column name of %s", table)
		out = append(out, name)
	}
	c.NoError(rows.Err(), "iterate the columns of %s", table)
	return out
}

// seed0068 inserts the fixture rows the conversion has to carry: turns (one with
// a response, a request_head_id and a real author, one 30 days old, one at
// ordinal 7), broadcasts and raw requests, some old enough to be compressed.
func seed0068(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (userID, convID string) {
	t.Helper()
	c := assert.NewAborting(t)

	for table, supplied := range fixtureColumns {
		for _, col := range requiredColumns(t, ctx, pool, table) {
			if !slices.Contains(supplied, col) {
				t.Fatalf("%s.%s is NOT NULL without a default and the fixture does not set it", table, col)
			}
		}
	}

	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.users (username, token_sha256)
		VALUES ('rafiki0068', 'h0068') RETURNING id::text`).Scan(&userID), "insert the fixture user")
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test', 'server') RETURNING id::text`).Scan(&convID), "insert the fixture conversation")

	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, model, response, request_head_id, author_user_id, created_at)
		VALUES
			($1::uuid, 7, 'm1', '{"stop_reason":"end_turn"}'::jsonb, gen_random_uuid(), $2::uuid, now()),
			($1::uuid, 8, 'm2', NULL, NULL, NULL, now() - interval '30 days'),
			($1::uuid, 9, 'm3', '{"second":true}'::jsonb, gen_random_uuid(), NULL, now())`, convID, userID)
	c.Require().NoError(err, "insert the fixture turns")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrouter.broadcast (session_id, model, created_at, raw_payload)
		VALUES ('s0068', 'm1', now(), '{"n":1}'::jsonb),
		       ('s0068', 'm2', now() - interval '30 days', '{"n":2}'::jsonb),
		       ('s0068', 'm3', now() - interval '40 days', '{"n":3}'::jsonb)`)
	c.Require().NoError(err, "insert the fixture broadcasts")

	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.raw_http_request (source, req_method, req_path, created_at)
		VALUES ('proxy', 'POST', '/v1/messages', now()),
		       ('proxy', 'POST', '/v1/messages', now() - interval '30 days')`)
	c.Require().NoError(err, "insert the fixture raw requests")

	// One compressed chunk makes the copy read through the decompression path.
	// The compression policy may already have run on the 30-day-old rows.
	var compressed bool
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM timescaledb_information.chunks
		                WHERE hypertable_name = 'broadcast' AND is_compressed)`).Scan(&compressed),
		"probe broadcast compression")
	if !compressed {
		_, err = pool.Exec(ctx, `
			SELECT compress_chunk(c, true) FROM show_chunks('openrouter.broadcast') c LIMIT 1`)
		c.Require().NoError(err, "compress one broadcast chunk")
	}
	return userID, convID
}

// tableState0068 is what the conversion must preserve for one table.
type tableState0068 struct {
	indexes []string
	owner   string
	rows    int64
}

// captureTableState0068 records a table's index names, owner and row count.
func captureTableState0068(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) tableState0068 {
	t.Helper()
	c := assert.NewAborting(t)
	var st tableState0068
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, table).Scan(&st.owner),
		"read the owner of %s", table)
	st.indexes = queryStrings(t, ctx, pool, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = split_part($1, '.', 1) AND tablename = split_part($1, '.', 2)
		 ORDER BY indexname`, table)
	st.rows = countOf(t, ctx, pool, `SELECT count(*) FROM `+table)
	return st
}

// queryStrings returns one string column of every row.
func queryStrings(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, sql, args...)
	c.Require().NoError(err, "query %q", sql)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		c.Require().NoError(rows.Scan(&s), "scan a row of %q", sql)
		out = append(out, s)
	}
	c.Require().NoError(rows.Err(), "iterate the rows of %q", sql)
	return out
}

// countOf runs a query that returns one count.
func countOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	c := assert.NewAborting(t)
	var n int64
	c.Require().NoError(pool.QueryRow(ctx, sql).Scan(&n), "count with %q", sql)
	return n
}

// isHypertable reports whether a schema-qualified table is a Timescale hypertable.
func isHypertable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	c := assert.NewAborting(t)
	var hyper bool
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables
		                WHERE hypertable_schema = split_part($1, '.', 1)
		                  AND hypertable_name = split_part($1, '.', 2))`, table).Scan(&hyper),
		"probe hypertable %s", table)
	return hyper
}

// migrationRecorded reports whether a version is recorded in the migrations table.
func migrationRecorded(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int) bool {
	t.Helper()
	c := assert.NewAborting(t)
	var recorded bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+migrationsTable+` WHERE version = $1)`, version).Scan(&recorded),
		"probe version %d", version)
	return recorded
}

// aclGrant is one grant on a relation as aclexplode reports it: the grantee's
// oid (0 is PUBLIC), the privilege, and whether it carries the grant option.
type aclGrant struct {
	grantee   int64
	privilege string
	grantable bool
}

// relationGrants reads a relation's grants structurally. Substring matching on
// relacl cannot tell PUBLIC's `=r/` from `pg_read_all_data=r/`, and
// has_table_privilege answers true for a predefined role that was never granted
// anything on the relation at all.
func relationGrants(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) []aclGrant {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT a.grantee::bigint, a.privilege_type, a.is_grantable
		  FROM pg_class c, aclexplode(c.relacl) a
		 WHERE c.oid = $1::regclass
		 ORDER BY 1, 2, 3`, rel)
	c.Require().NoError(err, "read the grants on %s", rel)
	defer rows.Close()
	var out []aclGrant
	for rows.Next() {
		var g aclGrant
		c.Require().NoError(rows.Scan(&g.grantee, &g.privilege, &g.grantable), "scan a grant on %s", rel)
		out = append(out, g)
	}
	c.Require().NoError(rows.Err(), "iterate the grants on %s", rel)
	return out
}

// roleOID reads a role's oid, which is what aclexplode reports as a grantee.
func roleOID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) int64 {
	t.Helper()
	c := assert.NewAborting(t)
	var oid int64
	c.Require().NoError(pool.QueryRow(ctx, `SELECT oid::bigint FROM pg_roles WHERE rolname = $1`, role).Scan(&oid),
		"read the oid of role %s", role)
	return oid
}

// relationSurface0068 is the state 0068 has to carry across the rewrite of a
// relation: who owns it, its comment, and its reloptions (view options).
type relationSurface0068 struct {
	owner      string
	comment    string
	reloptions string
}

// relationSurface reads a relation's owner, comment and reloptions.
func relationSurface(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) relationSurface0068 {
	t.Helper()
	c := assert.NewAborting(t)
	var s relationSurface0068
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT pg_get_userbyid(relowner),
		       coalesce(obj_description(oid, 'pg_class'), ''),
		       coalesce(array_to_string(reloptions, ','), '')
		  FROM pg_class WHERE oid = $1::regclass`, rel).Scan(&s.owner, &s.comment, &s.reloptions),
		"read the surface of %s", rel)
	return s
}

// scratchRoleName returns a role name unique to this run: roles are cluster-wide,
// so a fixed name would collide with a concurrent run.
func scratchRoleName() string {
	return fmt.Sprintf("rafiki_0068_owner_%d", time.Now().UnixNano())
}

// dropScratchRole removes a fixture role. Register it BEFORE the scratch database
// exists so it runs AFTER that database is dropped (cleanups are LIFO): by then
// the role owns nothing, so a plain DROP ROLE succeeds.
func dropScratchRole(t *testing.T, role string) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		return
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		// The role is cluster-wide: a silent failure here leaves it behind for
		// every later run to collide with, so say so instead of swallowing it.
		t.Logf("drop scratch role %s: connect: %v", role, err)
		return
	}
	defer admin.Close()
	var dropErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, dropErr = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role); dropErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("drop scratch role %s: %v", role, dropErr)
}

// adorn0068 gives the fixture state 0068 could not produce by accident: a
// comment on the table and on the view, a view option, grants that differ per
// grantee, and an owner that is NOT the migrating role. Without it, deleting the
// owner, comment, reloptions or regrant steps from the migration changes nothing
// a test can observe.
func adorn0068(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerRole string) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := pool.Exec(ctx, "CREATE ROLE "+ownerRole)
	c.Require().NoError(err, "create the fixture role")

	for _, stmt := range []string{
		`ALTER TABLE conversations.conversation_turn OWNER TO ` + ownerRole,
		`ALTER VIEW conversations.v_turn OWNER TO ` + ownerRole,
		`COMMENT ON TABLE conversations.conversation_turn IS '0068 fixture table comment'`,
		`COMMENT ON VIEW conversations.v_turn IS '0068 fixture view comment'`,
		`ALTER VIEW conversations.v_turn SET (security_invoker = true)`,
		`GRANT SELECT ON conversations.v_turn TO PUBLIC`,
		`GRANT SELECT ON conversations.v_turn TO pg_read_all_data WITH GRANT OPTION`,
		`GRANT SELECT ON conversations.raw_http_request TO PUBLIC`,
	} {
		_, err := pool.Exec(ctx, stmt)
		c.Require().NoError(err, "%s", stmt)
	}
}

// TestMigrate0068ConvertsHypertables is the conversion itself: three
// hypertables, with data (one chunk compressed), become ordinary tables that
// keep their rows, their response values, their index and constraint names,
// their identity sequence, their owner, their ACL and the v_turn view over them.
func TestMigrate0068ConvertsHypertables(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	// Registered before the pool so it runs after the scratch database is gone.
	ownerRole := scratchRoleName()
	t.Cleanup(func() { dropScratchRole(t, ownerRole) })
	pool := testPool(t)

	c.Require().NoError(migrateTo(ctx, pool, 67), "migrate to 0067")
	makeHypertables(t, ctx, pool)
	seed0068(t, ctx, pool)
	adorn0068(t, ctx, pool, ownerRole)

	// The migrating role is not the owner, so the owner assertions below can fail.
	var migrator string
	c.Require().NoError(pool.QueryRow(ctx, `SELECT current_user`).Scan(&migrator), "read the migrating role")
	c.NotEq(migrator, ownerRole, "the fixture owner must not be the migrating role")

	for _, ht := range hypertables0068 {
		c.True(isHypertable(t, ctx, pool, ht.table), "%s is not a hypertable before 0068; the fixture is wrong", ht.table)
	}

	// What the conversion must preserve, recorded before it runs.
	before := map[string]tableState0068{}
	for _, ht := range hypertables0068 {
		before[ht.table] = captureTableState0068(t, ctx, pool, ht.table)
	}
	responsesBefore := queryStrings(t, ctx, pool, `
		SELECT coalesce(response::text, '(null)') FROM conversations.conversation_turn ORDER BY id`)
	responseCountBefore := countOf(t, ctx, pool, `SELECT count(response) FROM conversations.conversation_turn`)
	turnsBefore := countOf(t, ctx, pool, `SELECT count(*) FROM conversations.conversation_turn`)
	vTurnBefore := queryStrings(t, ctx, pool, `
		SELECT v.id::text || '|' || v.model FROM conversations.v_turn v ORDER BY v.id`)
	vTurnCountBefore := countOf(t, ctx, pool, `SELECT count(*) FROM conversations.v_turn`)
	broadcastMaxBefore := countOf(t, ctx, pool, `SELECT coalesce(max(id), 0) FROM openrouter.broadcast`)
	turnSurfaceBefore := relationSurface(t, ctx, pool, "conversations.conversation_turn")
	vTurnSurfaceBefore := relationSurface(t, ctx, pool, "conversations.v_turn")
	vTurnGrantsBefore := relationGrants(t, ctx, pool, "conversations.v_turn")
	rawGrantsBefore := relationGrants(t, ctx, pool, "conversations.raw_http_request")

	c.Require().NoError(Migrate(ctx, pool), "migrate")

	// No hypertable left, and no policy job left for one either: the jobs belong
	// to the dropped hypertable.
	for _, ht := range hypertables0068 {
		c.False(isHypertable(t, ctx, pool, ht.table), "%s is still a hypertable after 0068", ht.table)
		var jobs int
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT count(*) FROM timescaledb_information.jobs
			 WHERE hypertable_schema = split_part($1, '.', 1)
			   AND hypertable_name = split_part($1, '.', 2)`, ht.table).Scan(&jobs),
			"probe the jobs of %s", ht.table)
		c.Eq(0, jobs, "%s still carries %d Timescale job(s)", ht.table, jobs)
	}

	// rafiki never drops the extension: it belongs to the operator.
	var exts int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`).Scan(&exts), "probe the extension")
	c.Eq(1, exts, "0068 must leave the timescaledb extension installed")

	// Rows, owner, every index name and the ANALYZE survive, under the same names.
	for _, ht := range hypertables0068 {
		after := captureTableState0068(t, ctx, pool, ht.table)
		c.Eq(before[ht.table].rows, after.rows, "%s lost rows", ht.table)
		c.Eq(before[ht.table].owner, after.owner, "%s changed owner", ht.table)
		c.EqDeep(before[ht.table].indexes, after.indexes, "%s indexes", ht.table)

		// ANALYZE ran. reltuples cannot show it — building the primary key sets it to
		// the row count as well (measured) — so the observable is the relation's own
		// analyze statistics: the twin is created inside 0068, so a non-zero
		// analyze_count can only be the migration's ANALYZE.
		var analyzeCount int64
		var lastAnalyze *time.Time
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT coalesce(analyze_count, 0), last_analyze FROM pg_stat_all_tables
			 WHERE relid = $1::regclass`, ht.table).Scan(&analyzeCount, &lastAnalyze),
			"read the analyze statistics of %s", ht.table)
		c.GreaterOrEqual(int64(1), analyzeCount, "%s was never analyzed by 0068", ht.table)
		c.NotNil(lastAnalyze, "%s has no last_analyze; 0068 did not analyze it", ht.table)
	}

	// The owner is the fixture's role, not the role that migrated, and the comment
	// came back with the table.
	turnSurfaceAfter := relationSurface(t, ctx, pool, "conversations.conversation_turn")
	c.Eq(ownerRole, turnSurfaceAfter.owner, "conversation_turn lost its owner")
	c.EqDeep(turnSurfaceBefore, turnSurfaceAfter, "conversation_turn's owner, comment or reloptions changed")
	c.Eq("0068 fixture table comment", turnSurfaceAfter.comment,
		"conversation_turn's comment is not the one the fixture set")

	// v_turn keeps its owner, its comment and its options: security_invoker lives
	// in reloptions, so a recreation that forgot WITH (...) loses it.
	vTurnSurfaceAfter := relationSurface(t, ctx, pool, "conversations.v_turn")
	c.EqDeep(vTurnSurfaceBefore, vTurnSurfaceAfter, "v_turn's owner, comment or reloptions changed")
	c.Eq(ownerRole, vTurnSurfaceAfter.owner, "v_turn lost its owner")
	c.Eq("security_invoker=true", vTurnSurfaceAfter.reloptions, "v_turn lost its security_invoker option")
	c.Eq("0068 fixture view comment", vTurnSurfaceAfter.comment, "v_turn lost its comment")
	c.Eq(turnsBefore, countOf(t, ctx, pool, `SELECT count(*) FROM conversations.conversation_turn`), "turn count")
	c.Eq(responseCountBefore, countOf(t, ctx, pool, `SELECT count(response) FROM conversations.conversation_turn`), "count(response)")
	c.EqDeep(responsesBefore, queryStrings(t, ctx, pool, `
		SELECT coalesce(response::text, '(null)') FROM conversations.conversation_turn ORDER BY id`),
		"the response values changed")

	// The primary key and the foreign key are constraints, not bare indexes, and
	// they are back under the names they were deployed with.
	for _, con := range []struct{ table, name, contype string }{
		{"conversations.conversation_turn", "conversation_turn_pkey", "p"},
		{"conversations.conversation_turn", "conversation_turn_author_user_id_fkey", "f"},
		{"openrouter.broadcast", "broadcast_pkey", "p"},
		{"conversations.raw_http_request", "raw_http_request_pkey", "p"},
	} {
		var contype string
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT contype::text FROM pg_constraint
			 WHERE conrelid = $1::regclass AND conname = $2`, con.table, con.name).Scan(&contype),
			"probe constraint %s on %s", con.name, con.table)
		c.Eq(con.contype, contype, "constraint %s on %s", con.name, con.table)
	}

	// The FK is enforced, not merely declared.
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn (conversation_id, ordinal, author_user_id)
		VALUES ($1::uuid, 10, '00000000-0000-0000-0000-000000000001'::uuid)`,
		queryStrings(t, ctx, pool, `SELECT conversation_id::text FROM conversations.conversation_turn LIMIT 1`)[0])
	c.Require().Error(err, "a turn referencing an unknown user was accepted; the FK is not enforced")
	var pgErr *pgconn.PgError
	c.True(errors.As(err, &pgErr), "the FK violation is not a *pgconn.PgError: %v", err)
	if pgErr != nil {
		c.Eq("23503", pgErr.Code, "foreign key violation SQLSTATE")
	}

	// The identity sequence kept its name and its position: the next broadcast
	// gets an id above every one the fixture inserted.
	var newID int64
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO openrouter.broadcast (session_id, created_at, raw_payload)
		VALUES ('after', now(), '{}'::jsonb) RETURNING id`).Scan(&newID), "insert a broadcast after 0068")
	c.Greater(broadcastMaxBefore, newID, "the identity sequence was reset; a new broadcast reused an id")
	var seq string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT pg_get_serial_sequence('openrouter.broadcast', 'id')`).Scan(&seq), "read the broadcast id sequence")
	c.True(strings.HasSuffix(seq, "broadcast_id_seq"), "the identity sequence is %q, want it named broadcast_id_seq", seq)

	// v_turn is selectable, identical, and still grants exactly what it granted.
	c.Eq(vTurnCountBefore, countOf(t, ctx, pool, `SELECT count(*) FROM conversations.v_turn`), "v_turn row count")
	c.EqDeep(vTurnBefore, queryStrings(t, ctx, pool, `
		SELECT v.id::text || '|' || v.model FROM conversations.v_turn v ORDER BY v.id`), "v_turn rows")

	// The grants come back one grantee at a time: PUBLIC is grantee oid 0, the
	// named role is its own oid, and the grant option rides the grant it was set
	// on. Both are read from aclexplode — has_table_privilege answers true for
	// pg_read_all_data with no grant at all, and a substring cannot tell the two
	// apart.
	public := int64(0)
	readAllData := roleOID(t, ctx, pool, "pg_read_all_data")
	vTurnGrantsAfter := relationGrants(t, ctx, pool, "conversations.v_turn")
	rawGrantsAfter := relationGrants(t, ctx, pool, "conversations.raw_http_request")
	c.EqDeep(vTurnGrantsBefore, vTurnGrantsAfter, "v_turn's grants changed")
	c.EqDeep(rawGrantsBefore, rawGrantsAfter, "raw_http_request's grants changed")
	c.Contains(vTurnGrantsAfter, aclGrant{public, "SELECT", false}, "v_turn lost its PUBLIC SELECT grant")
	c.Contains(vTurnGrantsAfter, aclGrant{readAllData, "SELECT", true},
		"v_turn lost pg_read_all_data's SELECT grant or its grant option")
	c.NotContains(vTurnGrantsAfter, aclGrant{readAllData, "SELECT", false},
		"pg_read_all_data's grant lost its grant option")
	c.Contains(rawGrantsAfter, aclGrant{public, "SELECT", false}, "raw_http_request lost its PUBLIC SELECT grant")

	// A second run has nothing to convert and changes nothing.
	oids := queryStrings(t, ctx, pool, `
		SELECT 'conversations.conversation_turn'::regclass::oid::text
		       || ',' || 'openrouter.broadcast'::regclass::oid::text
		       || ',' || 'conversations.raw_http_request'::regclass::oid::text`)
	c.Require().NoError(Migrate(ctx, pool), "second migrate")
	c.EqDeep(oids, queryStrings(t, ctx, pool, `
		SELECT 'conversations.conversation_turn'::regclass::oid::text
		       || ',' || 'openrouter.broadcast'::regclass::oid::text
		       || ',' || 'conversations.raw_http_request'::regclass::oid::text`),
		"a second Migrate re-created a table")
}

// TestMigrate0068IsANoOpOnPlainTables runs the migration's SQL a second time over
// plain tables. The oid assertion is what dies if the "is it a hypertable" guard
// is removed: converting a plain table would mint a new one.
func TestMigrate0068IsANoOpOnPlainTables(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	oidsBefore := queryStrings(t, ctx, pool, `
		SELECT 'conversations.conversation_turn'::regclass::oid::text
		       || ',' || 'openrouter.broadcast'::regclass::oid::text
		       || ',' || 'conversations.raw_http_request'::regclass::oid::text`)
	rowsBefore := map[string]int64{}
	for _, ht := range hypertables0068 {
		rowsBefore[ht.table] = countOf(t, ctx, pool, `SELECT count(*) FROM `+ht.table)
	}

	up, err := os.ReadFile("migrations/0068_plain_turn_tables.up.sql")
	c.Require().NoError(err, "read the up migration")
	_, err = pool.Exec(ctx, string(up))
	c.Require().NoError(err, "re-running 0068 over plain tables must be a no-op")

	c.EqDeep(oidsBefore, queryStrings(t, ctx, pool, `
		SELECT 'conversations.conversation_turn'::regclass::oid::text
		       || ',' || 'openrouter.broadcast'::regclass::oid::text
		       || ',' || 'conversations.raw_http_request'::regclass::oid::text`),
		"0068 rewrote a table that was already ordinary")
	for _, ht := range hypertables0068 {
		c.Eq(rowsBefore[ht.table], countOf(t, ctx, pool, `SELECT count(*) FROM `+ht.table), "%s row count", ht.table)
	}
}

// TestMigrate0068WithoutTimescaleDB pins the pg_extension guard: on a server with
// no timescaledb at all the migration must not reach for timescaledb_information.
func TestMigrate0068WithoutTimescaleDB(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPoolFromTemplate(t, "template0")

	var exts int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`).Scan(&exts), "probe the extension")
	c.Eq(0, exts, "the template0 scratch database already has timescaledb; the test proves nothing")

	c.Require().NoError(Migrate(ctx, pool), "migrate")
	c.True(migrationRecorded(t, ctx, pool, 68), "version 68 was not recorded")
	for _, ht := range hypertables0068 {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, ht.table).Scan(&exists),
			"probe %s", ht.table)
		c.True(exists, "%s does not exist after migrating a database without timescaledb", ht.table)
	}
}

// makeShortCopy injects a BEFORE INSERT trigger on
// conversations.conversation_turn_plain the moment the migration creates it, so
// the copy loses data. It has to be an event trigger: the twin does not exist
// before the migration runs.
func makeShortCopy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rowBody string) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION public.rafiki_test_short_row() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			%s
		END
		$fn$;
		CREATE FUNCTION public.rafiki_test_short() RETURNS event_trigger LANGUAGE plpgsql AS $fn$
		DECLARE
			cmd record;
		BEGIN
			FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
				IF cmd.object_identity = 'conversations.conversation_turn_plain' THEN
					EXECUTE 'CREATE OR REPLACE TRIGGER rafiki_test_short BEFORE INSERT ON conversations.conversation_turn_plain FOR EACH ROW EXECUTE FUNCTION public.rafiki_test_short_row()';
				END IF;
			END LOOP;
		END
		$fn$;
		CREATE EVENT TRIGGER rafiki_test_short ON ddl_command_end
			WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION public.rafiki_test_short();`, rowBody))
	c.Require().NoError(err, "create the short-copy event trigger")
}

// assertRolledBack checks what a refused migration must leave behind: the
// hypertable, its rows, no plain twin, and no recorded version.
func assertRolledBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rows int64) {
	t.Helper()
	c := assert.NewAborting(t)
	c.True(isHypertable(t, ctx, pool, "conversations.conversation_turn"),
		"the refused migration left conversation_turn an ordinary table")
	c.Eq(rows, countOf(t, ctx, pool, `SELECT count(*) FROM conversations.conversation_turn`),
		"the refused migration lost rows")
	var twin bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT to_regclass('conversations.conversation_turn_plain') IS NOT NULL`).Scan(&twin),
		"probe the plain twin")
	c.False(twin, "the refused migration left conversations.conversation_turn_plain behind")
	c.False(migrationRecorded(t, ctx, pool, 68), "the refused migration recorded version 68")
}

// TestMigrate0068RefusesAShortCopy pins the row-count verification: a copy that
// silently drops a row must abort the migration, not replace the table.
func TestMigrate0068RefusesAShortCopy(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)

	c.Require().NoError(migrateTo(ctx, pool, 67), "migrate to 0067")
	makeHypertables(t, ctx, pool)
	seed0068(t, ctx, pool)
	rows := countOf(t, ctx, pool, `SELECT count(*) FROM conversations.conversation_turn`)

	makeShortCopy(t, ctx, pool, `IF NEW.ordinal = 7 THEN RETURN NULL; END IF; RETURN NEW;`)

	err := Migrate(ctx, pool)
	c.Require().Error(err, "Migrate accepted a copy that lost a row")
	c.ErrorContains(err, "lost rows", "the refusal names the lost rows")
	assertRolledBack(t, ctx, pool, rows)
}

// TestMigrate0068RefusesAShortColumnCount pins the per-column verification: the
// row count matches, but a nullable column the migration counts lost a value,
// which must abort the migration too. Both columns in `counted` are exercised:
// response, and request_head_id.
func TestMigrate0068RefusesAShortColumnCount(t *testing.T) {
	for _, col := range []string{"response", "request_head_id"} {
		t.Run(col, func(t *testing.T) {
			c := assert.NewAborting(t)
			ctx := context.Background()
			pool := testPool(t)

			c.Require().NoError(migrateTo(ctx, pool, 67), "migrate to 0067")
			makeHypertables(t, ctx, pool)
			seed0068(t, ctx, pool)
			rows := countOf(t, ctx, pool, `SELECT count(*) FROM conversations.conversation_turn`)
			c.NotZero(countOf(t, ctx, pool, `SELECT count(`+col+`) FROM conversations.conversation_turn`),
				"the fixture has no %s value to lose", col)

			makeShortCopy(t, ctx, pool,
				fmt.Sprintf(`IF NEW.ordinal = 7 THEN NEW.%s := NULL; END IF; RETURN NEW;`, col))

			err := Migrate(ctx, pool)
			c.Require().Error(err, "Migrate accepted a copy that lost a %s value", col)
			c.ErrorContains(err, "lost column "+col, "the refusal names the lost column")
			assertRolledBack(t, ctx, pool, rows)
		})
	}
}

// TestMigrate0068RefusesAnUnknownDependent pins the missing CASCADE: a dependent
// object the migration does not know about fails it loudly instead of being
// dropped with the hypertable.
func TestMigrate0068RefusesAnUnknownDependent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)

	c.Require().NoError(migrateTo(ctx, pool, 67), "migrate to 0067")
	makeHypertables(t, ctx, pool)
	_, err := pool.Exec(ctx, `CREATE VIEW public.rafiki_test_dependent AS SELECT id FROM openrouter.broadcast`)
	c.Require().NoError(err, "create the dependent view")

	err = Migrate(ctx, pool)
	c.Require().Error(err, "Migrate dropped a hypertable an unknown view depended on")
	c.ErrorContains(err, "cannot drop table openrouter.broadcast", "the refusal names the table")
	// pgx renders a server error as severity + message + SQLSTATE, so the
	// dependent's name is in the DETAIL: assert on the whole server error.
	var pgErr *pgconn.PgError
	c.True(errors.As(err, &pgErr), "the refusal is not a *pgconn.PgError: %v", err)
	if pgErr != nil {
		c.StrContains(pgErr.Message+" "+pgErr.Detail, "rafiki_test_dependent",
			"the refusal does not name the dependent view")
	}
	c.True(isHypertable(t, ctx, pool, "openrouter.broadcast"),
		"the refused migration left openrouter.broadcast an ordinary table")
	c.False(migrationRecorded(t, ctx, pool, 68), "the refused migration recorded version 68")
}
