// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

// scratchPool gives the test its own database, migrated fresh from the
// embedded chain — the scratch-database pattern from
// pkg/presetsdb/postgres_test.go's testStore. A private database is what lets
// this test use the fixed child id c_1 (and fixed external_refs) without
// colliding with the shared test database other packages run against.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_close_stamp_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestCloseStampLinksFundiClaudeAndThreads pins the child↔conversation
// linkage Close stamps. A conversation is stamped closed when it is the
// child's own conversation (A — fundi children set child.conversation_id), or
// when its external_ref is the child id (B — a claude root thread) or
// child_id + ":" + thread (C — a claude subagent thread). D exists to fail an
// UNescaped LIKE: '_"' is a single-character wildcard, so 'c_1:%' matches
// cX1:t1 unless the pattern escapes the underscore. E is unrelated.
func TestCloseStampLinksFundiClaudeAndThreads(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()

	insertConversation := func(externalRef string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref)
			 VALUES ('test','server', NULLIF($1,'')) RETURNING id::text`, externalRef).Scan(&id); err != nil {
			t.Fatalf("insert conversation %q: %v", externalRef, err)
		}
		return id
	}

	convA := insertConversation("") // linked via child.conversation_id
	convB := insertConversation("c_1")
	convC := insertConversation("c_1:t1")
	convD := insertConversation("cX1:t1") // must NOT match LIKE 'c\_1:%'
	convE := insertConversation("unrelated")

	s := childstoredb.New(pool)
	if err := s.Upsert(ctx, childstore.ChildRecord{
		ChildID:        "c_1",
		ConversationID: convA,
		Kind:           protocol.KindFundi,
		Status:         string(protocol.StatusExited),
		SpawnedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("upsert child: %v", err)
	}

	c := &Controller{pool: pool}
	if err := c.stampConversationsClosed(ctx, "c_1"); err != nil {
		t.Fatalf("stampConversationsClosed: %v", err)
	}

	closedAt := func(id string) *time.Time {
		t.Helper()
		var ts *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT closed_at FROM conversations.conversation WHERE id = $1::uuid`, id).Scan(&ts); err != nil {
			t.Fatalf("read closed_at for %s: %v", id, err)
		}
		return ts
	}

	for id, label := range map[string]string{convA: "A (child.conversation_id)", convB: "B (external_ref = child)", convC: "C (external_ref = child:thread)"} {
		if closedAt(id) == nil {
			t.Errorf("conversation %s (%s) was not stamped closed", label, id)
		}
	}
	for id, label := range map[string]string{convD: "D (look-alike ref, needs the escaped LIKE)", convE: "E (unrelated)"} {
		if closedAt(id) != nil {
			t.Errorf("conversation %s (%s) must not be stamped closed", label, id)
		}
	}

	// Idempotent by construction: a second stamp leaves the first close time
	// in place rather than overwriting it.
	first := closedAt(convB)
	time.Sleep(2 * time.Millisecond)
	if err := c.stampConversationsClosed(ctx, "c_1"); err != nil {
		t.Fatalf("second stampConversationsClosed: %v", err)
	}
	if second := closedAt(convB); !second.Equal(*first) {
		t.Errorf("closed_at moved on re-stamp: %v -> %v", first, second)
	}
}
