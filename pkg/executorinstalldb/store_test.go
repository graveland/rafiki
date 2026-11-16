// SPDX-License-Identifier: Apache-2.0

// The conversations.executor_install store's conformance tests, against
// RAFIKI_TEST_DSN.
//
// The shared test database does NOT reset between runs, so every test here uses
// PER-RUN-UNIQUE ids, removes what it inserted in t.Cleanup, and NEVER asserts a
// global row count. Cleanup TOMBSTONES rather than DELETE'ing because this table
// carries removed_at (the operator rule: never delete data a table can
// tombstone); the per-run-unique ids are what keep one run's rows out of
// another's reads.
package executorinstalldb_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executorinstalldb"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func testStore(t *testing.T) (executorinstall.Store, *pgxpool.Pool) {
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
	return executorinstalldb.New(pool), pool
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

// newRow builds a fully-populated row for ownerID with unique id/executor.
func newRow(t *testing.T, ownerID string) executorinstall.Row {
	t.Helper()
	return executorinstall.Row{
		ID:                 "install-" + randHex(t),
		OwnerUserID:        ownerID,
		ExecutorID:         "exec-" + randHex(t),
		LauncherExecutorID: "launcher-" + randHex(t),
		Target:             "host-" + randHex(t),
		Instance:           "inst-" + randHex(t),
		Root:               "/srv/rafiki-" + randHex(t),
		InstalledByChild:   "child-" + randHex(t),
		Platform:           "linux/amd64",
	}
}

// insert inserts r and tombstones it on cleanup.
func insert(t *testing.T, s executorinstall.Store, r executorinstall.Row) executorinstall.Row {
	t.Helper()
	assert.NewAborting(t).NoError(s.Insert(context.Background(), r), "insert %s", r.ID)
	t.Cleanup(func() { _ = s.Tombstone(context.Background(), r.ID) })
	return r
}

func TestInstallStoreRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	r := newRow(t, owner(t))
	insert(t, s, r)

	got, ok, err := s.ByExecutor(ctx, r.ExecutorID)
	c.NoError(err, "by executor")
	c.True(ok, "ByExecutor(%s) not found", r.ExecutorID)

	c.Eq(r.ID, got.ID, "id")
	c.Eq(r.OwnerUserID, got.OwnerUserID, "owner_user_id")
	c.Eq(r.ExecutorID, got.ExecutorID, "executor_id")
	c.Eq(r.LauncherExecutorID, got.LauncherExecutorID, "launcher_executor_id")
	c.Eq(r.Target, got.Target, "target")
	c.Eq(r.Instance, got.Instance, "instance")
	c.Eq(r.Root, got.Root, "root")
	c.Eq(r.InstalledByChild, got.InstalledByChild, "installed_by_child")
	c.Eq(r.Platform, got.Platform, "platform")
	c.True(got.CreatedAt.After(time.Now().Add(-time.Minute)), "created_at not set by the db clock: %v", got.CreatedAt)
	c.True(got.RemovedAt == nil, "removed_at = %v, want nil", got.RemovedAt)

	// SetLauncher rewrites both fields on the live row.
	newLauncher, newPlatform := "launcher-"+randHex(t), "darwin/arm64"
	c.NoError(s.SetLauncher(ctx, r.ID, newLauncher, newPlatform), "set launcher")
	got, ok, err = s.ByExecutor(ctx, r.ExecutorID)
	c.NoError(err, "by executor after set launcher")
	c.True(ok, "row vanished after SetLauncher")
	c.Eq(newLauncher, got.LauncherExecutorID, "launcher_executor_id after SetLauncher")
	c.Eq(newPlatform, got.Platform, "platform after SetLauncher")
}

func TestInstallStoreSecondLiveRowIsErrExists(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	first := newRow(t, owner(t))
	insert(t, s, first)

	// Same executor, different row id: the live unique index rejects it.
	second := newRow(t, first.OwnerUserID)
	second.ExecutorID = first.ExecutorID
	c.ErrorIs(s.Insert(ctx, second), executorinstall.ErrExists,
		"a second live row for the same executor")

	// A different executor id is fine.
	third := newRow(t, first.OwnerUserID)
	insert(t, s, third)
}

func TestInstallStoreTombstoneFreesTheExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	s, pool := testStore(t)
	ctx := context.Background()

	first := newRow(t, owner(t))
	insert(t, s, first)

	c.NoError(s.Tombstone(ctx, first.ID), "tombstone")
	_, ok, err := s.ByExecutor(ctx, first.ExecutorID)
	c.NoError(err, "by executor after tombstone")
	c.False(ok, "ByExecutor returned a tombstoned row")

	// The tombstoned row SURVIVES — it is never deleted — and its stamp is the
	// first one: a second Tombstone leaves it in place.
	var stamped time.Time
	c.NoError(pool.QueryRow(ctx,
		`SELECT removed_at FROM conversations.executor_install WHERE id=$1`, first.ID).Scan(&stamped),
		"read tombstoned row")
	c.NoError(s.Tombstone(ctx, first.ID), "second tombstone")
	var again time.Time
	c.NoError(pool.QueryRow(ctx,
		`SELECT removed_at FROM conversations.executor_install WHERE id=$1`, first.ID).Scan(&again),
		"re-read tombstoned row")
	c.True(again.Equal(stamped), "removed_at moved from %v to %v", stamped, again)

	// An unknown id is a silent no-op.
	c.NoError(s.Tombstone(ctx, "no-such-"+randHex(t)), "tombstone unknown id")

	// The executor id is free again.
	reuse := newRow(t, first.OwnerUserID)
	reuse.ExecutorID = first.ExecutorID
	insert(t, s, reuse)
}

func TestInstallStoreListLiveScopesByOwner(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStore(t)
	ctx := context.Background()

	a, b := owner(t), owner(t)
	a1, a2 := newRow(t, a), newRow(t, a)
	b1 := newRow(t, b)
	insert(t, s, a1)
	insert(t, s, a2)
	insert(t, s, b1)

	ids := func(rows []executorinstall.Row) map[string]bool {
		out := make(map[string]bool, len(rows))
		for _, r := range rows {
			out[r.ID] = true
		}
		return out
	}

	rows, err := s.ListLive(ctx, a)
	c.NoError(err, "list live for owner a")
	got := ids(rows)
	c.True(got[a1.ID] && got[a2.ID], "ListLive(%s) omitted one of its own rows", a)
	c.False(got[b1.ID], "ListLive(%s) returned another owner's row", a)
	for _, r := range rows {
		c.Eq(a, r.OwnerUserID, "ListLive returned another owner's row")
	}

	// "" is every owner, and must contain all three.
	rows, err = s.ListLive(ctx, "")
	c.NoError(err, "list live for every owner")
	got = ids(rows)
	c.True(got[a1.ID] && got[a2.ID] && got[b1.ID], "ListLive(\"\") omitted an inserted live row")

	// A tombstoned row drops out of its owner's list.
	c.NoError(s.Tombstone(ctx, a1.ID), "tombstone a1")
	rows, err = s.ListLive(ctx, a)
	c.NoError(err, "list live after tombstone")
	got = ids(rows)
	c.False(got[a1.ID], "ListLive included a tombstoned row")
	c.True(got[a2.ID], "ListLive dropped a live row")
}
