package childstoredb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// scratchPool gives the test its own database, migrated fresh from the
// embedded chain — the scratch-database pattern from
// pkg/presetsdb/postgres_test.go's testStore, so the test never touches a
// shared database.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_childstoredb_%d", time.Now().UnixNano())
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

func TestUpsertAndList(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_" + time.Now().Format("20060102150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID:   id,
		Kind:      protocol.KindFundi,
		Name:      "worker",
		Cwd:       "/tmp/work",
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),
		DaemonID:  "daemon-a",
		MaxCost:   5,
		Labels:    map[string]string{"owner": "brent"},
		Config:    childstore.ChildConfig{SystemPrompt: "sys"},
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got := findRecord(t, s, id)
	if got.Name != "worker" {
		t.Errorf("Name = %q, want %q", got.Name, "worker")
	}
	if got.Labels["owner"] != "brent" {
		t.Errorf("Labels = %v, want owner=brent", got.Labels)
	}
	if got.Config.SystemPrompt != "sys" {
		t.Errorf("Config.SystemPrompt = %q, want %q", got.Config.SystemPrompt, "sys")
	}
	if got.MaxCost != 5 {
		t.Errorf("MaxCost = %v, want 5", got.MaxCost)
	}
}

// TestUpsertPreservesLastStatus is the regression test for design §1.5's first
// COALESCE. last_status is written once, by the exit path; an ordinary status
// upsert that blanked it would leave the recovery predicate with nothing to
// read and silently stop auto-resuming every child.
func TestUpsertPreservesLastStatus(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_laststatus_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	base := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusExited), SpawnedAt: time.Now(),
	}

	withLast := base
	withLast.LastStatus = string(protocol.StatusIdle)
	if err := s.Upsert(ctx, withLast); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// An ordinary write carrying no LastStatus must not erase it.
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	got := findRecord(t, s, id)
	if got.LastStatus != string(protocol.StatusIdle) {
		t.Errorf("LastStatus = %q, want %q — the COALESCE is missing", got.LastStatus, protocol.StatusIdle)
	}
}

// TestUpsertPreservesConversationID is the regression test for the second
// COALESCE. conversation_id becomes known after the row already exists; a later
// upsert that has not re-read it must not erase the correlation.
func TestUpsertPreservesConversationID(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	convID := insertConversation(t, pool)
	id := "c_convid_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	base := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusIdle), SpawnedAt: time.Now(),
	}
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	withConv := base
	withConv.ConversationID = convID
	if err := s.Upsert(ctx, withConv); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("third Upsert: %v", err)
	}

	got := findRecord(t, s, id)
	if got.ConversationID != convID {
		t.Errorf("ConversationID = %q, want %q — the COALESCE is missing", got.ConversationID, convID)
	}
}

func TestDelete(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_delete_" + time.Now().Format("150405.000000")
	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusExited), SpawnedAt: time.Now(),
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := lookup(t, s, id); ok {
		t.Error("record still present after Delete")
	}
	// Idempotent: deleting a missing row is not an error.
	if err := s.Delete(ctx, id); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// TestStoreDeleteStampsClosedAt pins the close tombstone: after Delete the
// row's closed_at is stamped and List omits it. Fails if the SQL still names
// the pre-0036 column — that UPDATE errors against the renamed column, it
// cannot silently match nothing.
func TestStoreDeleteStampsClosedAt(t *testing.T) {
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_closedat_" + time.Now().Format("150405.000000")
	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusExited), SpawnedAt: time.Now(),
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var closedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT closed_at FROM conversations.child WHERE child_id = $1`, id).Scan(&closedAt); err != nil {
		t.Fatalf("read closed_at: %v", err)
	}
	if closedAt == nil {
		t.Fatal("closed_at is NULL after Delete — the close stamp is missing")
	}
	if _, ok := lookup(t, s, id); ok {
		t.Error("List returned a closed row — the closed_at filter is missing")
	}
}

func findRecord(t *testing.T, s *Store, id string) childstore.ChildRecord {
	t.Helper()
	rec, ok := lookup(t, s, id)
	if !ok {
		t.Fatalf("record %q not found", id)
	}
	return rec
}

func lookup(t *testing.T, s *Store, id string) (childstore.ChildRecord, bool) {
	t.Helper()
	recs, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range recs {
		if r.ChildID == id {
			return r, true
		}
	}
	return childstore.ChildRecord{}, false
}

func insertConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

// TestAdoptOwnership pins the ownership stamp's column contract: daemon_id and
// the rafiki/daemon label move together, existing labels survive the merge,
// status is untouched, and the row stays visible to List (closed_at is not
// the stamp's business).
func TestAdoptOwnership(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_" + time.Now().Format("20060102150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID:  id,
		Kind:     protocol.KindFundi,
		Status:   string(protocol.StatusExited),
		DaemonID: "daemon-a",
		Labels:   map[string]string{"rafiki/parent": "c_root", "rafiki/daemon": "daemon-a"},
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.AdoptOwnership(ctx, id, "daemon-b"); err != nil {
		t.Fatalf("AdoptOwnership: %v", err)
	}

	got := findRecord(t, s, id)
	if got.DaemonID != "daemon-b" {
		t.Errorf("DaemonID = %q, want %q", got.DaemonID, "daemon-b")
	}
	if got.Labels["rafiki/daemon"] != "daemon-b" {
		t.Errorf("rafiki/daemon label = %q, want %q (Close's gate reads the label)",
			got.Labels["rafiki/daemon"], "daemon-b")
	}
	if got.Labels["rafiki/parent"] != "c_root" {
		t.Errorf("labels were replaced, not merged: %v", got.Labels)
	}
	if got.Status != string(protocol.StatusExited) {
		t.Errorf("Status = %q, want %q — row content belongs to the child's own writes",
			got.Status, string(protocol.StatusExited))
	}
}

// TestAdoptOwnershipSkipsATombstonedRow: the stamp must never un-tombstone.
// A row closed in the race between List and the stamp stays closed.
func TestAdoptOwnershipSkipsATombstonedRow(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_" + time.Now().Format("20060102150405.000001")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID:  id,
		Kind:     protocol.KindFundi,
		Status:   string(protocol.StatusExited),
		DaemonID: "daemon-a",
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if err := s.AdoptOwnership(ctx, id, "daemon-b"); err != nil {
		t.Fatalf("AdoptOwnership: %v", err)
	}

	if _, ok := lookup(t, s, id); ok {
		t.Fatalf("adopting a tombstoned row resurrected it")
	}
}
