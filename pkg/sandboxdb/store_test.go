// SPDX-License-Identifier: Apache-2.0

// The conversations.sandbox store's conformance tests, against RAFIKI_TEST_DSN.
//
// The shared test database does NOT reset between runs and its rows are never
// cleaned, so every test here: uses a PER-RUN-UNIQUE owner id (and names), so
// its ListLive/GetByName can never see another run's rows; tombstones what it
// inserted in t.Cleanup; and NEVER asserts a global row count (ListAllLive is
// only checked for containment). This is the seedRecallWindow discipline.
package sandboxdb_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/sandboxdb"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func testStore(t *testing.T) (sandbox.Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return sandboxdb.NewPostgresStore(pool), pool
}

func randHex(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	assert.NewAborting(t).NoError(err, "rand")
	return hex.EncodeToString(b[:])
}

// owner returns a per-run-unique owner id, so no read here can collide with a
// previous run's rows in the shared database.
func owner(t *testing.T) string { return "owner-" + randHex(t) }

// insert inserts r and tombstones it on cleanup, so a live name it holds is
// freed for the next run.
func insert(t *testing.T, s sandbox.Store, r sandbox.Row) sandbox.Row {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(s.Insert(context.Background(), r), "insert %s", r.ID)
	t.Cleanup(func() {
		_ = s.MarkRemoved(context.Background(), r.ID, time.Now())
	})
	return r
}

// newRow builds a fully-populated row for owner o with a unique id/name.
func newRow(t *testing.T, o string) sandbox.Row {
	t.Helper()
	id := "sbx-" + randHex(t)
	return sandbox.Row{
		ID:                 id,
		OwnerUserID:        o,
		Name:               "name-" + randHex(t),
		ExecutorID:         "exec-" + randHex(t),
		LauncherExecutorID: "launcher-" + randHex(t),
		ContainerID:        "container-" + randHex(t),
		Image:              "alpine:3",
		Spec:               []byte(`{"image":"alpine:3","network":"none"}`),
		CreatedBy:          "child-" + randHex(t),
		OwnerChild:         "",
		Scope:              protocol.ScopeSelf,
		State:              "ready",
	}
}

func TestInsertGetRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	r := newRow(t, owner(t))
	r.OwnerChild = "owning-" + randHex(t)
	r.Scope = protocol.ScopeSubtree
	r.ExpiresAt = &exp
	insert(t, s, r)

	got, ok, err := s.Get(ctx, r.ID)
	c.NoError(err, "get")
	c.True(ok, "Get(%s) not found", r.ID)

	c.Eq(r.ID, got.ID, "id")
	c.Eq(r.OwnerUserID, got.OwnerUserID, "owner_user_id")
	c.Eq(r.Name, got.Name, "name")
	c.Eq(r.ExecutorID, got.ExecutorID, "executor_id")
	c.Eq(r.LauncherExecutorID, got.LauncherExecutorID, "launcher_executor_id")
	c.Eq(r.ContainerID, got.ContainerID, "container_id")
	c.Eq(r.Image, got.Image, "image")
	c.Eq(r.CreatedBy, got.CreatedBy, "created_by")
	c.Eq(r.OwnerChild, got.OwnerChild, "owner_child")
	c.Eq(protocol.ScopeSubtree, got.Scope, "scope")
	c.Eq("ready", got.State, "state")
	c.True(got.CreatedAt.After(time.Now().Add(-time.Minute)), "created_at not set by the db clock: %v", got.CreatedAt)
	c.True(got.ExpiresAt != nil && got.ExpiresAt.Equal(exp), "expires_at = %v, want %v", got.ExpiresAt, exp)
	c.True(got.RemovedAt == nil, "removed_at = %v, want nil", got.RemovedAt)

	// spec is jsonb, which normalises whitespace and key order — compare it
	// decoded, not byte-for-byte.
	var want, have map[string]any
	c.NoError(json.Unmarshal(r.Spec, &want), "unmarshal want")
	c.NoError(json.Unmarshal(got.Spec, &have), "unmarshal have")
	c.True(reflect.DeepEqual(want, have), "spec = %s, want %s", got.Spec, r.Spec)
}

func TestGetMissingIsNotAnError(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	_, ok, err := s.Get(context.Background(), "no-such-"+randHex(t))
	c.NoError(err, "get missing")
	c.False(ok, "Get of a missing id reported ok")
}

func TestInsertDuplicateLiveNameIsErrNameTaken(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	o := owner(t)

	first := newRow(t, o)
	insert(t, s, first)

	second := newRow(t, o)
	second.Name = first.Name // same owner, same live name
	err := s.Insert(context.Background(), second)
	c.True(errors.Is(err, sandbox.ErrNameTaken), "Insert of a duplicate live name = %v, want ErrNameTaken", err)

	// A DIFFERENT name under the same owner is fine.
	third := newRow(t, o)
	insert(t, s, third)
}

func TestRemovedNameIsFreed(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()
	o := owner(t)

	first := newRow(t, o)
	insert(t, s, first)
	c.NoError(s.MarkRemoved(ctx, first.ID, time.Now()), "mark removed")

	// The same name is now insertable.
	reuse := newRow(t, o)
	reuse.Name = first.Name
	c.NoError(s.Insert(ctx, reuse), "reusing a freed name")
	t.Cleanup(func() { _ = s.MarkRemoved(context.Background(), reuse.ID, time.Now()) })

	// The tombstoned row SURVIVES — Get still returns it.
	got, ok, err := s.Get(ctx, first.ID)
	c.NoError(err, "get removed")
	c.True(ok, "removed row was not returned by Get")
	c.True(got.RemovedAt != nil, "removed_at = nil on a tombstoned row")
}

func TestListLiveExcludesRemoved(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()
	o := owner(t)

	live := newRow(t, o)
	gone := newRow(t, o)
	insert(t, s, live)
	insert(t, s, gone)
	c.NoError(s.MarkRemoved(ctx, gone.ID, time.Now()), "mark removed")

	rows, err := s.ListLive(ctx, o)
	c.NoError(err, "list live")
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		c.Eq(o, r.OwnerUserID, "ListLive returned another owner's row")
		ids[r.ID] = true
	}
	c.True(ids[live.ID], "ListLive omitted the live row")
	c.False(ids[gone.ID], "ListLive included the removed row")
}

func TestListAllLiveContainsInserted(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	r := newRow(t, owner(t))
	insert(t, s, r)

	rows, err := s.ListAllLive(context.Background())
	c.NoError(err, "list all live")
	found := false
	for _, row := range rows {
		if row.ID == r.ID {
			found = true
			break
		}
	}
	c.True(found, "ListAllLive omitted an inserted live row")
}

func TestMarkRemovedTwiceKeepsFirstTimestamp(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	r := newRow(t, owner(t))
	insert(t, s, r)

	first := time.Now().Add(-time.Minute).UTC()
	c.NoError(s.MarkRemoved(ctx, r.ID, first), "first MarkRemoved")
	got, _, err := s.Get(ctx, r.ID)
	c.NoError(err, "get after first")
	c.True(got.RemovedAt != nil && got.RemovedAt.Equal(first), "removed_at = %v, want %v", got.RemovedAt, first)

	// A second call with a later time must not move the tombstone.
	c.NoError(s.MarkRemoved(ctx, r.ID, time.Now()), "second MarkRemoved")
	got, _, err = s.Get(ctx, r.ID)
	c.NoError(err, "get after second")
	c.True(got.RemovedAt != nil && got.RemovedAt.Equal(first), "removed_at moved to %v, want %v", got.RemovedAt, first)

	// An unknown id is a silent no-op, not an error.
	c.NoError(s.MarkRemoved(ctx, "no-such-"+randHex(t), time.Now()), "MarkRemoved unknown id")
}

func TestSetContainerAndState(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	r := newRow(t, owner(t))
	insert(t, s, r)

	c.NoError(s.SetContainer(ctx, r.ID, "ctr-"+randHex(t)), "set container")
	c.NoError(s.SetState(ctx, r.ID, "creating"), "set state creating")

	got, ok, err := s.Get(ctx, r.ID)
	c.NoError(err, "get")
	c.True(ok, "not found")
	c.Eq("creating", got.State, "state")
	c.True(got.ContainerID != r.ContainerID, "container_id was not updated")
}

func TestSetStateRejectsUnknownState(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	r := newRow(t, owner(t))
	insert(t, s, r)

	err := s.SetState(ctx, r.ID, "bogus")
	c.Error(err, "SetState accepted an unknown state; the CHECK constraint is missing")

	// The row is untouched, and a known state still works.
	c.NoError(s.SetState(ctx, r.ID, "lost"), "set state lost")
	got, _, err := s.Get(ctx, r.ID)
	c.NoError(err, "get")
	c.Eq("lost", got.State, "state after rejected write")
}

func TestSetStateAndContainerSkipTombstoned(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	r := newRow(t, owner(t))
	insert(t, s, r)

	// Tombstone the row, then attempt writes a stale snapshot could issue.
	c.NoError(s.MarkRemoved(ctx, r.ID, time.Now()), "mark removed")
	c.NoError(s.SetState(ctx, r.ID, "lost"), "SetState on a tombstoned row must not error")
	c.NoError(s.SetContainer(ctx, r.ID, "ctr-"+randHex(t)), "SetContainer on a tombstoned row must not error")

	got, ok, err := s.Get(ctx, r.ID)
	c.NoError(err, "get")
	c.True(ok, "not found")
	c.Eq("ready", got.State, "SetState wrote to a tombstoned row")
	c.Eq(r.ContainerID, got.ContainerID, "SetContainer wrote to a tombstoned row")
}

func TestGetByName(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()
	o := owner(t)

	named := newRow(t, o)
	insert(t, s, named)

	got, ok, err := s.GetByName(ctx, o, named.Name)
	c.NoError(err, "get by name")
	c.True(ok, "GetByName(%s) not found", named.Name)
	c.Eq(named.ID, got.ID, "id")

	// A spawn block (empty name) is never resolved by name.
	block := newRow(t, o)
	block.Name = ""
	block.OwnerChild = "child-" + randHex(t)
	block.Scope = protocol.ScopeSubtree
	insert(t, s, block)
	_, ok, err = s.GetByName(ctx, o, "")
	c.NoError(err, "get by empty name")
	c.False(ok, "GetByName matched a spawn block with an empty name")

	// A removed row is not returned by name.
	c.NoError(s.MarkRemoved(ctx, named.ID, time.Now()), "mark removed")
	_, ok, err = s.GetByName(ctx, o, named.Name)
	c.NoError(err, "get removed by name")
	c.False(ok, "GetByName returned a removed row")
}
