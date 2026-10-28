// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// The migrator tests need a real TimescaleDB (>= 2.22, PostgreSQL 18 for
// uuidv7()). Set RAFIKI_TEST_DSN to run them, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./pkg/store/...
//
// Each subtest runs in its own scratch database created from that DSN.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)
	return pool
}

// assertBaselineSchema checks the chain actually produced the conversations
// schema: the three tables the baseline creates, plus the provenance and
// prefix_hash columns later migrations depend on. Asserted directly against the
// catalog so a migration that records itself without doing its DDL is caught.
func assertBaselineSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	c := assert.NewCollecting(t)
	for _, table := range []string{"conversation", "conversation_turn", "conversation_attachment"} {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT to_regclass('conversations.'||$1) IS NOT NULL`, table).Scan(&exists), "probe conversations.%s", table)
		c.True(exists, "conversations.%s missing after Migrate", table)
	}
	// author_user_id, not author: 0019 replaced the free-text column with the
	// users FK. author_kind survives — it is a role marker, not an identity.
	for _, col := range []string{"source", "author_user_id", "author_kind", "prefix_hash"} {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema='conversations' AND table_name='conversation_turn'
			   AND column_name=$1)`, col).Scan(&exists), "probe conversation_turn.%s", col)
		c.True(exists, "conversation_turn.%s missing after Migrate", col)
	}
}

func baselineName(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		"SELECT name FROM "+migrationsTable+" WHERE version=$1", baselineVersion,
	).Scan(&name), "read baseline row")
	return name
}

func TestMigrateFreshDatabase(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()

	c.Require().NoError(Migrate(ctx, pool), "Migrate (fresh)")
	assertBaselineSchema(t, ctx, pool)
	c.Eq("baseline", baselineName(t, ctx, pool), "baseline row name")

	// The executed schema must actually work: uuidv7 default, hypertable insert.
	var convID string
	err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID)
	c.Require().NoError(err, "insert conversation")
	_, err = pool.Exec(ctx, `INSERT INTO conversations.conversation_turn (conversation_id, ordinal, request, prefix_hash)
		VALUES ($1::uuid, 0, '{}'::jsonb, 'abc')`, convID)
	c.Require().NoError(err, "insert turn")

	// Re-run is a no-op.
	c.Require().NoError(Migrate(ctx, pool), "Migrate (re-run)")
}

// TestMigrateConcurrent runs Migrate from two pools against the same fresh
// database: the advisory lock must serialize them, both must return nil, and
// the chain must be applied exactly once.
func TestMigrateConcurrent(t *testing.T) {
	c := assert.NewAborting(t)
	pool1 := testPool(t)
	ctx := context.Background()

	// Second pool onto the SAME scratch database as pool1.
	cfg := pool1.Config().Copy()
	pool2, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "second pool")
	t.Cleanup(pool2.Close)

	errs := make(chan error, 2)
	for _, p := range []*pgxpool.Pool{pool1, pool2} {
		go func(p *pgxpool.Pool) { errs <- Migrate(ctx, p) }(p)
	}
	for range 2 {
		c.NoError(<-errs, "concurrent Migrate")
	}

	chain, err := loadMigrations()
	c.NoError(err)
	var n, distinct int
	c.NoError(pool1.QueryRow(ctx, `SELECT count(*), count(DISTINCT version) FROM `+migrationsTable).Scan(&n, &distinct), "count versions")
	if n != len(chain) || distinct != len(chain) {
		t.Fatalf("chain recorded %d rows / %d versions, want %d each (applied exactly once)", n, distinct, len(chain))
	}
	assertBaselineSchema(t, ctx, pool1)
}

func TestMigrate0018UsersTable(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	// The table exists with the columns the auth path reads.
	var cols int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='users'
		   AND column_name IN ('id','username','token_sha256','created_at','deleted_at')`,
	).Scan(&cols), "probe users columns")
	c.Eq(5, cols, "users columns")

	// The digest is globally unique: two users can never share a token.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('a','dup'),('b','dup')`); err == nil {
		t.Fatal("duplicate token_sha256 was accepted; the UNIQUE constraint is missing")
	}

	// Usernames are unique among ACTIVE users only, so a name is reusable
	// after a tombstone. This is what makes `user rm` + `user create` a
	// working rotation story.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256, deleted_at)
		 VALUES ('brent','h1', now())`); err != nil {
		t.Fatalf("insert tombstoned user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('brent','h2')`); err != nil {
		t.Fatalf("reusing a tombstoned username must be allowed: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('brent','h3')`)
	c.Error(err, "two ACTIVE users share a username; the partial unique index is missing")
}

func TestMigrate0019UserAttribution(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	ck.NoError(Migrate(ctx, pool), "migrate")

	// The free-text identity columns are gone.
	for _, c := range []struct{ table, col string }{
		{"conversation", "owner"},
		{"conversation_turn", "author"},
	} {
		var exists bool
		ck.NoError(pool.QueryRow(ctx, `
			SELECT count(*) > 0 FROM information_schema.columns
			 WHERE table_schema='conversations' AND table_name=$1 AND column_name=$2`,
			c.table, c.col).Scan(&exists), "probe %s.%s", c.table, c.col)
		ck.False(exists, "conversations.%s.%s still exists", c.table, c.col)
	}

	// author_kind survives: it is a ROLE marker, not an identity, and it is
	// what separates human turns from the agent's own.
	var kindExists bool
	ck.NoError(pool.QueryRow(ctx, `
		SELECT count(*) > 0 FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='conversation_turn'
		   AND column_name='author_kind'`).Scan(&kindExists), "probe author_kind")
	ck.True(kindExists, "author_kind was dropped; it is a role marker and must survive")

	// Both FKs exist and point at users.
	var fks int
	ck.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.table_constraints tc
		  JOIN information_schema.constraint_column_usage ccu
		    ON ccu.constraint_name = tc.constraint_name
		 WHERE tc.table_schema='conversations' AND tc.constraint_type='FOREIGN KEY'
		   AND ccu.table_name='users'`).Scan(&fks), "probe fks")
	ck.GreaterOrEqual(2, fks, "found")

	// The views were rebuilt and resolve the username through the join.
	var uid, cid string
	ck.NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('brent','h1') RETURNING id::text`).Scan(&uid), "insert user")
	ck.NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (owner_user_id, driven_by, origin_entrypoint)
		 VALUES ($1::uuid,'server','test') RETURNING id::text`, uid).Scan(&cid), "insert conversation")
	var username string
	ck.NoError(pool.QueryRow(ctx,
		`SELECT owner_username FROM conversations.v_conversation WHERE id=$1::uuid`,
		cid).Scan(&username), "select v_conversation.owner_username")
	ck.Eq("brent", username, "owner_username")

	// The turn-level FK resolves the same way, through its own join.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.conversation_turn (conversation_id, ordinal, request, author_user_id)
		 VALUES ($1::uuid, 0, '{}'::jsonb, $2::uuid)`, cid, uid); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	var author string
	ck.NoError(pool.QueryRow(ctx,
		`SELECT author_username FROM conversations.v_turn WHERE conversation_id=$1::uuid`,
		cid).Scan(&author), "select v_turn.author_username")
	ck.Eq("brent", author, "author_username")

	// The FK is enforced inside the hypertable's chunks, not merely declared:
	// adding the constraint separately from the column (the only form
	// columnstore accepts) must still reject an unknown user.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.conversation_turn (conversation_id, ordinal, request, author_user_id)
		 VALUES ($1::uuid, 1, '{}'::jsonb, '00000000-0000-0000-0000-000000000001'::uuid)`, cid); err == nil {
		t.Fatal("a turn referencing an unknown user was accepted; the hypertable FK is not enforced")
	}

	// A tombstoned user still resolves — that is the point of not deleting.
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.users SET deleted_at=now() WHERE id=$1::uuid`, uid); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	ck.NoError(pool.QueryRow(ctx,
		`SELECT owner_username FROM conversations.v_conversation WHERE id=$1::uuid`,
		cid).Scan(&username), "select after tombstone")
	ck.Eq("brent", username, "owner_username after tombstone")

	// The guessing heuristics are gone: a users row answers what they used
	// to infer from the shape of a string.
	var heuristics int
	ck.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name IN ('v_conversation','v_turn')
		   AND column_name IN ('owner_canonical','owner_kind','owner','author')`).Scan(&heuristics), "probe view columns")
	ck.Eq(0, heuristics, "%d free-text owner columns survive in the views", heuristics)

	// v_analysis and v_finding do not depend on v_conversation (pg_depend says
	// only v_turn does), so the CASCADE must not have reached them.
	for _, v := range []string{"v_conversation", "v_turn", "v_analysis", "v_finding"} {
		var ok bool
		ck.NoError(pool.QueryRow(ctx,
			`SELECT to_regclass('conversations.'||$1) IS NOT NULL`, v).Scan(&ok), "probe %s", v)
		ck.True(ok, "view conversations.%s was not recreated", v)
	}

	// conversation_turn is still a hypertable with columnstore enabled: the
	// two-statement column+constraint form exists to avoid downgrading it.
	var compressed bool
	ck.NoError(pool.QueryRow(ctx, `
		SELECT compression_enabled FROM timescaledb_information.hypertables
		 WHERE hypertable_schema='conversations' AND hypertable_name='conversation_turn'`,
	).Scan(&compressed), "probe hypertable")
	ck.True(compressed, "conversation_turn lost its columnstore hypertable status")
}

// TestMigrate0037ServedProvider pins 0037: the chain produces a nullable,
// default-less served_provider on conversation_turn — the OpenRouter provider
// that served each turn, NULL meaning not reported — and its down migration
// actually drops it.
func TestMigrate0037ServedProvider(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	var typ, nullable, hasDefault string
	c.NoError(pool.QueryRow(ctx, `
		SELECT data_type, is_nullable, coalesce(column_default,'')
		  FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='conversation_turn'
		   AND column_name='served_provider'`).Scan(&typ, &nullable, &hasDefault), "probe served_provider")
	c.False(typ != "text" || nullable != "YES" || hasDefault != "", "served_provider = (%s, nullable=%s, default=%q), want (text, YES, no default)", typ, nullable, hasDefault)

	// There is no MigrateTo API, so exercise the down migration directly:
	// read the file (its absence or a no-op body must fail this test, not
	// silently restore to 0036 with the column still there) and run it.
	down, err := os.ReadFile("migrations/0037_turn_served_provider.down.sql")
	c.NoError(err, "read down migration")
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	var exists bool
	c.NoError(pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='conversation_turn'
		   AND column_name='served_provider')`).Scan(&exists), "probe served_provider after down")
	c.False(exists, "served_provider survived the down migration")
}

// TestMigrate0047Sandbox pins 0047: the chain produces conversations.sandbox
// with the columns the store reads, both partial indexes, and a state CHECK —
// and the down migration actually drops the table.
func TestMigrate0047Sandbox(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	var cols int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='sandbox'
		   AND column_name IN ('id','owner_user_id','name','executor_id','launcher_executor_id',
		       'container_id','image','spec','created_by','owner_child','scope','state',
		       'created_at','expires_at','removed_at')`).Scan(&cols), "probe sandbox columns")
	c.Eq(15, cols, "sandbox columns")

	var idx int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname='conversations' AND tablename='sandbox'
		   AND indexname IN ('sandbox_owner_name_unique','sandbox_owner_child_live')`).Scan(&idx), "probe sandbox indexes")
	c.Eq(2, idx, "sandbox indexes")

	insert := func(id, owner, name, state string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.sandbox
			   (id, owner_user_id, name, executor_id, launcher_executor_id, image, spec, state)
			 VALUES ($1,$2,$3,'e','l','alpine','{}'::jsonb,$4)`, id, owner, name, state)
		return err
	}

	// The state CHECK rejects an unknown state.
	if err := insert("s1", "o1", "n1", "bogus"); err == nil {
		t.Fatal("sandbox accepted state 'bogus'; the CHECK constraint is missing")
	}
	c.NoError(insert("s2", "o1", "n2", "ready"), "insert a valid row")

	// The (owner, name) unique index constrains LIVE rows only: a duplicate
	// live name is refused, a different name is fine, and a tombstone frees one.
	if err := insert("s3", "o1", "n2", "ready"); err == nil {
		t.Fatal("a second live (owner, name) was accepted; the partial unique index is missing")
	}
	c.NoError(insert("s4", "o1", "n3", "ready"), "a different live name is fine")
	// Empty names (spawn blocks) are excluded from the index, so two coexist.
	c.NoError(insert("s5", "o1", "", "ready"), "an empty name is unconstrained")
	c.NoError(insert("s6", "o1", "", "ready"), "a second empty name is unconstrained")
	if _, err := pool.Exec(ctx, `UPDATE conversations.sandbox SET removed_at=now() WHERE id='s2'`); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	c.NoError(insert("s7", "o1", "n2", "ready"), "reusing a freed name")

	// There is no MigrateTo API, so exercise the down migration directly.
	down, err := os.ReadFile("migrations/0047_sandbox.down.sql")
	c.NoError(err, "read down migration")
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	var exists bool
	c.NoError(pool.QueryRow(ctx, `SELECT to_regclass('conversations.sandbox') IS NOT NULL`).Scan(&exists), "probe sandbox after down")
	c.False(exists, "conversations.sandbox survived the down migration")
}

// TestMigrate0054RenamesLiveDuplicates pins 0054's rename pass — the only
// irreversible, data-mutating SQL in the chain. Migrate has already run, so
// drop the index it built, seed the duplicates a pre-0054 database could hold,
// then apply the migration's own SQL by reading the file: a future edit to it
// must keep this test green, and a no-op body must fail it.
//
// Seeded, all children of ONE owner:
//   - three LIVE rows named <tag>dup, in spawned_at order;
//   - three LIVE rows named <tag>tri, one carrying the legacy fundi/parent
//     label, so the group is split across both parent spellings and must still
//     be grouped as siblings;
//   - two CLOSED rows named <tag>dup, which the index exempts and the pass
//     must not touch.
//
// The renamed rows' conversations are linked by the three arms the pass
// handles: conversation_id, external_ref = child_id, and external_ref =
// child_id:<thread>. The third arm reaches the row through a LIKE pattern with
// an escaped `%`/`_`: child ids carry an underscore, so an unescaped pattern
// would over-match. Nothing here acts as a DECOY for that over-match yet — the
// escaping itself is not pinned by this test.
func TestMigrate0054RenamesLiveDuplicates(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	// Migrate left 0054's index in place; remove it so the duplicates this
	// migration exists to repair can be seeded.
	_, err := pool.Exec(ctx, `DROP INDEX conversations.child_sibling_name_live`)
	c.NoError(err, "drop child_sibling_name_live so duplicates can be seeded")

	tag := fmt.Sprintf("d54%d", time.Now().UnixNano())
	dup, tri, parent := tag+"dup", tag+"tri", tag+"parent"
	owner := insertMigrationOwner(t, ctx, pool, tag)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insertChild := func(childID, name, labels string, spawnedAt time.Time, closed bool) {
		t.Helper()
		var closedAt any
		if closed {
			closedAt = time.Now()
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO conversations.child
			   (child_id, owner_user_id, kind, name, labels, status, spawned_at, closed_at)
			 VALUES ($1, $2, 'fundi', $3, $4::jsonb, 'exited', $5, $6)`,
			childID, owner, name, labels, spawnedAt, closedAt); err != nil {
			t.Fatalf("insert child %s: %v", childID, err)
		}
	}
	parentLabel := func(key string) string { return fmt.Sprintf(`{"%s":"%s"}`, key, parent) }

	insertChild(dup+"_a", dup, `{}`, base, false)
	insertChild(dup+"_b", dup, `{}`, base.Add(time.Minute), false)
	insertChild(dup+"_c", dup, `{}`, base.Add(2*time.Minute), false)

	insertChild(tri+"_a", tri, parentLabel("rafiki/parent"), base, false)
	insertChild(tri+"_b", tri, parentLabel("fundi/parent"), base.Add(time.Minute), false)
	insertChild(tri+"_c", tri, parentLabel("rafiki/parent"), base.Add(2*time.Minute), false)

	insertChild(dup+"_closed_a", dup, `{}`, base, true)
	insertChild(dup+"_closed_b", dup, `{}`, base.Add(time.Minute), true)

	newConversation := func(name, externalRef *string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref, name)
			VALUES ('claude', 'server', $1, $2) RETURNING id::text`, externalRef, name).Scan(&id); err != nil {
			t.Fatalf("insert conversation %v: %v", externalRef, err)
		}
		return id
	}
	str := func(s string) *string { return &s }

	// Arm 1: the child row names its conversation directly.
	viaConversationID := newConversation(str(dup), nil)
	if _, err := pool.Exec(ctx, `UPDATE conversations.child SET conversation_id = $1 WHERE child_id = $2`,
		viaConversationID, dup+"_b"); err != nil {
		t.Fatalf("link conversation_id: %v", err)
	}
	// Arm 2: external_ref is the child id itself.
	viaChildID := newConversation(str(dup), str(dup+"_c"))
	// Arm 3: external_ref is child_id:<thread>, the claude-thread shape.
	viaThread := newConversation(str(tri), str(tri+"_b:thread1"))

	up, err := os.ReadFile("migrations/0054_child_sibling_name.up.sql")
	c.NoError(err, "read 0054 up migration")
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("apply 0054 up: %v", err)
	}

	// The oldest keeps its name; the later ones become <name>-2, -3.
	c.Eq(dup, childNameRow(t, ctx, pool, dup+"_a"), "the earliest spawned sibling keeps its name")
	c.Eq(dup+"-2", childNameRow(t, ctx, pool, dup+"_b"), "the second is renamed")
	c.Eq(dup+"-3", childNameRow(t, ctx, pool, dup+"_c"), "the third is renamed")
	c.Eq(tri, childNameRow(t, ctx, pool, tri+"_a"), "the split group's earliest keeps its name")
	c.Eq(tri+"-2", childNameRow(t, ctx, pool, tri+"_b"), "a legacy fundi/parent sibling is grouped and renamed")
	c.Eq(tri+"-3", childNameRow(t, ctx, pool, tri+"_c"), "the split group's third is renamed")
	c.Eq(dup, childNameRow(t, ctx, pool, dup+"_closed_a"), "a closed row is exempt from the rename")
	c.Eq(dup, childNameRow(t, ctx, pool, dup+"_closed_b"), "a second closed row keeps the name too")

	// Every arm's conversation follows its child's new name.
	c.Eq(dup+"-2", conversationNameRow(t, ctx, pool, viaConversationID), "the conversation linked by conversation_id is renamed")
	c.Eq(dup+"-3", conversationNameRow(t, ctx, pool, viaChildID), "the conversation linked by external_ref = child_id is renamed")
	c.Eq(tri+"-2", conversationNameRow(t, ctx, pool, viaThread), "the conversation linked by external_ref = child_id:<thread> is renamed")

	var exists bool
	c.NoError(pool.QueryRow(ctx, `SELECT to_regclass('conversations.child_sibling_name_live') IS NOT NULL`).Scan(&exists),
		"probe the index")
	c.True(exists, "the index was not created by the up migration")

	// And it is live again: a NEW live duplicate is refused, while a closed
	// one is still exempt.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, owner_user_id, kind, name, status, spawned_at)
		VALUES ($1, $2, 'fundi', $3, 'exited', now())`, dup+"_after", owner, dup); err == nil {
		t.Fatal("a new live duplicate name was accepted; the index is missing or wrong")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, owner_user_id, kind, name, status, spawned_at, closed_at)
		VALUES ($1, $2, 'fundi', $3, 'exited', now(), now())`, dup+"_after_closed", owner, dup); err != nil {
		t.Fatalf("a closed duplicate must stay exempt from the index: %v", err)
	}
}

// childNameRow reads one child row's name.
func childNameRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, childID string) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, `SELECT coalesce(name,'') FROM conversations.child WHERE child_id = $1`, childID).Scan(&name); err != nil {
		t.Fatalf("read the name for %s: %v", childID, err)
	}
	return name
}

// conversationNameRow reads one conversation row's name.
func conversationNameRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, `SELECT coalesce(name,'') FROM conversations.conversation WHERE id = $1::uuid`, id).Scan(&name); err != nil {
		t.Fatalf("read the name for conversation %s: %v", id, err)
	}
	return name
}

// insertMigrationOwner creates one active user and returns its id, which is what
// the sibling index groups by.
func insertMigrationOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.users (username, token_sha256) VALUES ($1, $1) RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("insert owner %q: %v", name, err)
	}
	return id
}

// TestMigrate0057TreeColumns pins 0057-0059: the tree columns on the message,
// conversation and turn tables, the four counters, and the unique path index —
// exercised rather than merely catalogued, because an index that exists but is
// not unique would still satisfy information_schema.
func TestMigrate0057TreeColumns(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	for _, col := range []struct{ table, name string }{
		{"conversation_message", "parent_id"},
		{"conversation_message", "derived_from_id"},
		{"conversation_message", "path_hash"},
		{"conversation_message", "hash_version"},
		{"conversation_message", "depth"},
		{"conversation_message", "provider_message_id"},
		{"conversation", "head_id"},
		{"conversation", "next_ordinal"},
		{"conversation", "node_count"},
		{"conversation", "fork_count"},
		{"conversation", "fallback_count"},
		{"conversation", "payload_bytes"},
		{"conversation_turn", "request_head_id"},
	} {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                WHERE table_schema='conversations' AND table_name=$1 AND column_name=$2)`,
			col.table, col.name).Scan(&exists), "probe %s.%s", col.table, col.name)
		c.True(exists, "conversations.%s.%s missing after Migrate", col.table, col.name)
	}

	var unique bool
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT i.indisunique
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_namespace n ON n.oid = ic.relnamespace
		 WHERE n.nspname = 'conversations' AND ic.relname = 'conversation_message_path_uniq'`).Scan(&unique),
		"probe conversation_message_path_uniq")
	c.True(unique, "conversation_message_path_uniq is not unique")

	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID), "insert conversation")

	insertNode := func(ordinal int, pathHash []byte, hashVersion *int16) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_message
			   (conversation_id, ordinal, role, content, path_hash, hash_version)
			VALUES ($1::uuid, $2, 'user', '[{"type":"text","text":"hi"}]'::jsonb, $3, $4)`,
			convID, ordinal, pathHash, hashVersion)
		return err
	}
	version := int16(1)

	c.Require().NoError(insertNode(0, []byte{0x01, 0x02}, &version), "insert the first node")

	// One path is one node per conversation: a second claim on it is refused by
	// the unique index, and the refusal is the 23505 a caller must retry on.
	err := insertNode(1, []byte{0x01, 0x02}, &version)
	c.Error(err, "a second node with the same path_hash was accepted")
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		c.Eq("23505", pgErr.Code, "duplicate path_hash sqlstate")
	} else {
		c.True(false, "duplicate path_hash err = %v, want a *pgconn.PgError", err)
	}

	// Legacy rows carry no tree position, and NULLs are distinct in a unique
	// index — so any number of them coexist.
	c.Require().NoError(insertNode(2, nil, nil), "insert a legacy row")
	c.Require().NoError(insertNode(3, nil, nil), "insert a second legacy row")

	// path_hash and hash_version move together.
	err = insertNode(4, []byte{0x03}, nil)
	c.Error(err, "path_hash without hash_version was accepted; the hash_pair CHECK is missing")

	// There is no MigrateTo API, so exercise the down migration directly: a
	// no-op body must fail this test, not silently leave the tree in place.
	down, err := os.ReadFile("migrations/0057_conversation_tree.down.sql")
	c.Require().NoError(err, "read down migration")
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	for _, col := range []struct{ table, name string }{
		{"conversation_message", "parent_id"},
		{"conversation_message", "path_hash"},
		{"conversation", "head_id"},
		{"conversation", "next_ordinal"},
	} {
		var exists bool
		c.Require().NoError(pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                WHERE table_schema='conversations' AND table_name=$1 AND column_name=$2)`,
			col.table, col.name).Scan(&exists), "probe %s.%s after down", col.table, col.name)
		c.False(exists, "conversations.%s.%s survived the down migration", col.table, col.name)
	}
}

// TestMigrateGoStepRunsInMigrationTransaction pins the contract the tree
// backfill depends on: a Go step runs INSIDE its migration's transaction, so a
// failing step leaves neither the SQL nor the version recorded. A step that ran
// after the commit — or in a transaction of its own — would leave a database
// that believes it is migrated with half the work done.
func TestMigrateGoStepRunsInMigrationTransaction(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)

	chain, err := loadMigrations()
	c.Require().NoError(err, "load chain")
	head := chain[len(chain)-1]
	// 0059 is the migration whose rollback is observed below; a new head means
	// this test must be repointed, not silently left asserting an older one.
	c.Eq(59, head.version, "this test observes 0059's rollback")

	prev, hadPrev := goSteps[head.version]
	goSteps[head.version] = func(context.Context, pgx.Tx) error { return errors.New("step refuses") }
	t.Cleanup(func() {
		if hadPrev {
			goSteps[head.version] = prev
			return
		}
		delete(goSteps, head.version)
	})

	c.Require().NoError(migrateTo(ctx, pool, head.version-1), "migrate to one version below the head")

	err = Migrate(ctx, pool)
	c.Error(err, "Migrate must fail when the head's go step fails")
	c.True(err != nil && strings.Contains(err.Error(), "go step"), "error names the go step: %v", err)

	var recorded bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+migrationsTable+` WHERE version=$1)`, head.version).Scan(&recorded),
		"probe the migrations table")
	c.False(recorded, "version %d was recorded despite its go step failing", head.version)

	var nodeCount bool
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_schema='conversations' AND table_name='conversation'
		                  AND column_name='node_count')`).Scan(&nodeCount), "probe node_count")
	c.False(nodeCount, "0059's column survived a failed go step; the SQL did not roll back with it")
}

// TestMigrateGoStepUnknownVersion pins loadMigrations' refusal of a step for a
// version the chain does not have: such a step never runs, so the feature it
// implements would silently not exist.
func TestMigrateGoStepUnknownVersion(t *testing.T) {
	c := assert.NewAborting(t)

	goSteps[9999] = func(context.Context, pgx.Tx) error { return nil }
	t.Cleanup(func() { delete(goSteps, 9999) })

	_, err := loadMigrations()
	c.Error(err, "a go step for an unknown version must fail the chain")
	c.True(err != nil && strings.Contains(err.Error(), "unknown version 9999"), "error names the version: %v", err)
}
