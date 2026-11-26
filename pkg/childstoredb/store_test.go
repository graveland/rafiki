package childstoredb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pool")
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	t.Cleanup(pool.Close)
	return pool
}

// scratchPool gives the test its own database, migrated fresh from the
// embedded chain — the scratch-database pattern from
// pkg/presetsdb/postgres_test.go's testStore, so the test never touches a
// shared database.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_childstoredb_%d", time.Now().UnixNano())
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

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool
}

func TestUpsertAndList(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert")

	got := findRecord(t, s, id)
	c.Eq("worker", got.Name, "Name")
	c.Eq("brent", got.Labels["owner"], "Labels = %v, want owner=brent", got.Labels)
	c.Eq("sys", got.Config.SystemPrompt, "Config.SystemPrompt")
	c.Eq(5, got.MaxCost, "MaxCost")
}

// TestUpsertPreservesLastStatus is the regression test for design §1.5's first
// COALESCE. last_status is written once, by the exit path; an ordinary status
// upsert that blanked it would leave the recovery predicate with nothing to
// read and silently stop auto-resuming every child.
func TestUpsertPreservesLastStatus(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(s.Upsert(ctx, withLast), "first Upsert")

	// An ordinary write carrying no LastStatus must not erase it.
	c.Require().NoError(s.Upsert(ctx, base), "second Upsert")

	got := findRecord(t, s, id)
	c.Eq(string(protocol.StatusIdle), got.LastStatus, "LastStatus = %q, want %q — the COALESCE is missing", got.LastStatus, protocol.StatusIdle)
}

// TestUpsertPreservesConversationID is the regression test for the second
// COALESCE. conversation_id becomes known after the row already exists; a later
// upsert that has not re-read it must not erase the correlation.
func TestUpsertPreservesConversationID(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(s.Upsert(ctx, base), "first Upsert")

	withConv := base
	withConv.ConversationID = convID
	c.Require().NoError(s.Upsert(ctx, withConv), "second Upsert")

	c.Require().NoError(s.Upsert(ctx, base), "third Upsert")

	got := findRecord(t, s, id)
	c.Eq(convID, got.ConversationID, "ConversationID")
}

func TestDelete(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_delete_" + time.Now().Format("150405.000000")
	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusExited), SpawnedAt: time.Now(),
	}
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert")
	c.Require().NoError(s.Delete(ctx, id), "Delete")
	_, ok := lookup(t, s, id)
	c.False(ok, "record still present after Delete")
	// Idempotent: deleting a missing row is not an error.
	c.NoError(s.Delete(ctx, id), "second Delete")
}

// TestStoreDeleteStampsClosedAt pins the close tombstone: after Delete the
// row's closed_at is stamped and List omits it. Fails if the SQL still names
// the pre-0036 column — that UPDATE errors against the renamed column, it
// cannot silently match nothing.
func TestStoreDeleteStampsClosedAt(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_closedat_" + time.Now().Format("150405.000000")
	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status: string(protocol.StatusExited), SpawnedAt: time.Now(),
	}
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert")
	c.Require().NoError(s.Delete(ctx, id), "Delete")

	var closedAt *time.Time
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT closed_at FROM conversations.child WHERE child_id = $1`, id).Scan(&closedAt), "read closed_at")
	c.Require().NotNil(closedAt, "closed_at is NULL after Delete — the close stamp is missing")
	_, ok := lookup(t, s, id)
	c.False(ok, "List returned a closed row — the closed_at filter is missing")
}

// TestUpsertAndListCarrySkipDerivedIndex pins the flag on both halves of the
// round trip: it must be in the upsert's INSERT and UPDATE lists AND in the
// List scan, or a later upsert clears it or a resume reads it back false.
func TestUpsertAndListCarrySkipDerivedIndex(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	idOn := "c_skipderived_on_" + time.Now().Format("150405.000000")
	idOff := "c_skipderived_off_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, idOn); _ = s.Delete(ctx, idOff) })

	on := childstore.ChildRecord{
		ChildID: idOn, Kind: protocol.KindFundi,
		Status: string(protocol.StatusIdle), SpawnedAt: time.Now(),
		SkipDerivedIndex: true,
	}
	c.Require().NoError(s.Upsert(ctx, on), "Upsert on")
	c.True(findRecord(t, s, idOn).SkipDerivedIndex, "SkipDerivedIndex true did not survive upsert+list")

	off := childstore.ChildRecord{
		ChildID: idOff, Kind: protocol.KindFundi,
		Status: string(protocol.StatusIdle), SpawnedAt: time.Now(),
	}
	c.Require().NoError(s.Upsert(ctx, off), "Upsert off")
	c.False(findRecord(t, s, idOff).SkipDerivedIndex, "SkipDerivedIndex defaulted true")

	// A later upsert that changes an unrelated field must not clear the flag —
	// the column is missing from the DO UPDATE SET list if this fails.
	on.Name = "renamed"
	on.Status = string(protocol.StatusExited)
	c.Require().NoError(s.Upsert(ctx, on), "second Upsert on")
	c.True(findRecord(t, s, idOn).SkipDerivedIndex, "SkipDerivedIndex was cleared by a later upsert")
}

func findRecord(t *testing.T, s *Store, id string) childstore.ChildRecord {
	t.Helper()
	rec, ok := lookup(t, s, id)
	assert.NewAborting(t).True(ok, "record %q not found", id)
	return rec
}

func lookup(t *testing.T, s *Store, id string) (childstore.ChildRecord, bool) {
	t.Helper()
	recs, err := s.List(context.Background())
	assert.NewAborting(t).NoError(err, "List")
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
	assert.NewAborting(t).NoError(err, "insert conversation")
	return id
}

// TestAdoptOwnership pins the ownership stamp's column contract: daemon_id and
// the rafiki/daemon label move together, existing labels survive the merge,
// status is untouched, and the row stays visible to List (closed_at is not
// the stamp's business).
func TestAdoptOwnership(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert")

	c.Require().NoError(s.AdoptOwnership(ctx, id, "daemon-b"), "AdoptOwnership")

	got := findRecord(t, s, id)
	c.Eq("daemon-b", got.DaemonID, "DaemonID")
	c.Eq("daemon-b", got.Labels["rafiki/daemon"], "rafiki/daemon label")
	c.Eq("c_root", got.Labels["rafiki/parent"], "labels were replaced, not merged: %v", got.Labels)
	c.Eq(string(protocol.StatusExited), got.Status, "Status")
}

// TestAdoptOwnershipSkipsATombstonedRow: the stamp must never un-tombstone.
// A row closed in the race between List and the stamp stays closed.
func TestAdoptOwnershipSkipsATombstonedRow(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(s.Upsert(ctx, rec), "Upsert")
	c.NoError(s.Delete(ctx, id), "Delete")

	c.NoError(s.AdoptOwnership(ctx, id, "daemon-b"), "AdoptOwnership")

	_, ok := lookup(t, s, id)
	c.False(ok, "adopting a tombstoned row resurrected it")
}

// Every set shape survives the INSERT of a fresh row, which is the only full-row
// write of executor_set (the conflict branch leaves it to SetExecutorSet; see
// TestFullRowUpsertNeverUndoesASetUpdate).
func TestExecutorSetRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	steps := []struct {
		name string
		set  []string
	}{
		{"nil", nil},
		{"ids", []string{"a", "b"}},
		{"empty", []string{}},
		{"all", []string{childstore.ExecutorSetAll}},
	}
	for i, tc := range steps {
		id := fmt.Sprintf("c_%s_%d", time.Now().Format("20060102150405.000000"), i)
		t.Cleanup(func() { _ = s.Delete(ctx, id) })
		rec := childstore.ChildRecord{
			ChildID:     id,
			Kind:        protocol.KindFundi,
			Name:        "set-" + id,
			Status:      string(protocol.StatusIdle),
			SpawnedAt:   time.Now(),
			DaemonID:    "daemon-a",
			ExecutorSet: tc.set,
		}
		c.Require().NoError(s.Upsert(ctx, rec), "Upsert "+tc.name)

		got := findRecord(t, s, id)
		if tc.set == nil {
			c.True(got.ExecutorSet == nil, "%s: nil must stay nil, got %#v", tc.name, got.ExecutorSet)
			continue
		}
		c.True(got.ExecutorSet != nil, "%s: must be non-nil", tc.name)
		c.EqDeep(tc.set, got.ExecutorSet, tc.name)
	}
}

// TestUpsertRefuseIfClosedLeavesTheTombstone pins the one row write that may NOT
// clear closed_at: a native thread child's, whose id is deterministic
// (<parent>:<thread>) and so can address a closed row on a later turn of the
// same thread. The guard is the DATABASE's — the conflict update is gated on
// closed_at IS NULL — so it holds even when the caller's own "is this row
// closed?" read came back stale, which is the interleaving this exists for.
//
// The second half pins that the exception stays an exception: the SAME write
// without the flag keeps the clear-on-write semantics every other caller has.
func TestUpsertRefuseIfClosedLeavesTheTombstone(t *testing.T) {
	c := assert.NewAborting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	id := fmt.Sprintf("c_refuseclosed_%d", time.Now().UnixNano())
	rec := childstore.ChildRecord{
		ChildID:   id,
		Kind:      protocol.KindClaude,
		Name:      "task:t1",
		Status:    string(protocol.StatusExited),
		SpawnedAt: time.Now(),
		DaemonID:  "daemon-a",
		// The row IS the only native marker recovery reads, so the label is set and
		// the flag is what the write path asks for.
		Labels:         map[string]string{"rafiki/parent": "c_parent", "rafiki/root": "c_parent"},
		RefuseIfClosed: false,
	}
	c.Require().NoError(s.Upsert(ctx, rec), "first write")
	c.Require().NoError(s.Delete(ctx, id), "tombstone the row")

	live := rec
	live.Status = string(protocol.StatusIdle)
	live.RefuseIfClosed = true
	err := s.Upsert(ctx, live)
	c.True(errors.Is(err, childstore.ErrClosed), "err = %v, want childstore.ErrClosed", err)

	closed, err := s.IsClosed(ctx, id)
	c.Require().NoError(err, "is closed")
	c.True(closed, "a refused write must leave the tombstone standing")
	// List skips closed rows by design, so the status is read directly.
	var status string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT status FROM conversations.child WHERE child_id = $1`, id).Scan(&status), "read the closed row")
	c.Eq(string(protocol.StatusExited), status, "the refused write must not have landed its status either")

	plain := rec
	plain.Status = string(protocol.StatusIdle)
	c.Require().NoError(s.Upsert(ctx, plain), "an ordinary write still clears closed_at")
	closed, err = s.IsClosed(ctx, id)
	c.Require().NoError(err, "is closed after the ordinary write")
	c.False(closed, "the general Upsert semantics are unchanged for every other caller")
}
