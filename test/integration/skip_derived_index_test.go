// SPDX-License-Identifier: Apache-2.0

package integration_test

// skip_derived_index inheritance and persistence, end to end against a REAL
// daemon. A spawn ORs the flag into a parented request and the resolved value
// is stored on conversations.child.skip_derived_index, so a child can switch
// derived indexing off for its whole subtree and can never switch it back on
// beneath a parent that has it off.
//
// Only this file can prove the wiring: the OR lives in Controller.Spawn
// (cmd/rafikid/controller.go's inheritSkipDerivedIndex) and the persistence is
// childstoredb's upsert, neither of which a unit fixture reaches through the
// real Connect Spawn path. Same rules as the other DB-backed daemon tests:
// source RAFIKI_TEST_DSN, use per-run daemon ids, and read conversations.child
// directly for the stored value — the RowList/Summary wire carries no flag.

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// spawnChildWithSkip spawns a fundi child over the daemon's control socket,
// setting SkipDerivedIndex on the request and, when parentID is non-empty,
// recording it as the child's tree edge. It is spawnChild/spawnChildUnder with
// the flag the tests here exercise; the control UDS is the operator credential
// (local trust), the same path those helpers spawn through.
func spawnChildWithSkip(t *testing.T, d *daemon, parentID string, skip bool) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:              "/tmp",
		NoSession:        true,
		Kind:             protocol.KindFundi,
		Model:            "anthropic/sonnet-latest",
		ParentChildId:    parentID,
		SkipDerivedIndex: skip,
	}))
	c.NoError(err, "spawn (parent=%q, skip=%v) failed", parentID, skip)
	c.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

// childSkipDerivedIndex reads a spawned child's persisted flag straight from
// conversations.child. It polls briefly for the row: the read races nothing in
// a correct daemon (the upsert runs inside Spawn), but a missing row would
// otherwise fail with a confusing Scan error rather than a clear message.
func childSkipDerivedIndex(t *testing.T, pool *pgxpool.Pool, childID string) bool {
	t.Helper()
	c := assert.NewAborting(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var v bool
		err := pool.QueryRow(context.Background(),
			`SELECT skip_derived_index FROM conversations.child WHERE child_id = $1`, childID).Scan(&v)
		if err == nil {
			return v
		}
		if time.Now().After(deadline) {
			c.NoError(err, "read skip_derived_index for child %s", childID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSkipDerivedIndexInheritedThroughSpawn: a top-level child spawned with the
// flag set is stored true, and a child spawned beneath it with the request flag
// false is ALSO true — the parent's value is OR'd in. Dies if either the
// inheritSkipDerivedIndex OR or the upsert's persistence is missing.
func TestSkipDerivedIndexInheritedThroughSpawn(t *testing.T) {
	ck := assert.NewAborting(t)
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()

	d := bootDaemonDB(t, nextDaemonID())

	parent := spawnChildWithSkip(t, d, "", true)
	child := spawnChildWithSkip(t, d, parent, false)

	ck.True(childSkipDerivedIndex(t, pool, parent),
		"top-level child %s spawned with skip_derived_index=true stored false", parent)
	ck.True(childSkipDerivedIndex(t, pool, child),
		"child %s under flagged parent %s must inherit skip_derived_index=true", child, parent)
}

// TestSkipDerivedIndexDefaultStaysFalse: the zero value is "derive as today".
// A top-level child spawned without the flag, and a child spawned under it, are
// both stored false.
func TestSkipDerivedIndexDefaultStaysFalse(t *testing.T) {
	ck := assert.NewAborting(t)
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()

	d := bootDaemonDB(t, nextDaemonID())

	parent := spawnChildWithSkip(t, d, "", false)
	child := spawnChildWithSkip(t, d, parent, false)

	ck.False(childSkipDerivedIndex(t, pool, parent),
		"top-level child %s spawned without the flag must default to false", parent)
	ck.False(childSkipDerivedIndex(t, pool, child),
		"child %s under unflagged parent %s must default to false", child, parent)
}

// TestSkipDerivedIndexChildCannotClear: a child of a flagged parent cannot
// switch derived indexing back on — a request that explicitly sends false is
// still stored true, because the parent's value is OR'd in.
func TestSkipDerivedIndexChildCannotClear(t *testing.T) {
	ck := assert.NewAborting(t)
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()

	d := bootDaemonDB(t, nextDaemonID())

	parent := spawnChildWithSkip(t, d, "", true)
	child := spawnChildWithSkip(t, d, parent, false)

	ck.True(childSkipDerivedIndex(t, pool, child),
		"a child of flagged parent %s sent skip_derived_index=false must still store true", parent)
}

// TestSkipDerivedIndexSurvivesDaemonRestart is intentionally absent: the brief
// conditions it on the harness already having a restart helper used by the
// DB-state tests, and it does not. Every DB-state restart test (e.g.
// TestDBChildState_RestartSurvivesWipedStateDir, TestDBChildState_ResumesAfterDaemonCrash
// in db_child_state_test.go) re-inlines the same exec.Command(daemonBinary())
// boot rather than calling a shared helper. Writing a fourth copy here would
// add a restart fixture, not exercise a distinct persistence path — the value
// is already read back from conversations.child, which is exactly what a
// restart reads. Skipped per the brief.
