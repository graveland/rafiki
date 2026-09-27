// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestRecoveryScopingLeavesAnotherDaemonsChildAlone is the live proof for
// docs/plans/2026-08-30-rafiki-recovery-scoping-design.md.
//
// Daemon A owns a live fundi child. Daemon B boots against the same database
// and walks the same shared child table — childstoredb's listSQL is
// `FROM conversations.child` with no WHERE clause, so B sees every one of A's
// rows and calls recoverOne on each. B must leave A's child, and its inbox,
// exactly where they were.
//
// Verified empirically before this test was written: a child spawned with
// noSession:true still gets a conversation_id AND a live lease held by its
// daemon (holder=<daemonA>, ~5 minutes remaining), so no seeding is required —
// the natural post-spawn state is exactly the state under test.
//
// Scope note: this is an end-to-end safety confirmation, NOT the
// mutation-sensitive regression test. Removing the ownership gate does not
// make this fail, because the inbox is independently protected for a
// foreign-LIVE child (OnConversationResolved refuses the lease before
// resetUnconfirmedOnOwnership runs, and holdsLease gates replayInbox). The
// mutation-sensitive test is
// TestRecoverOneDoesNotAttemptToResumeAnotherDaemonsLiveChild in
// cmd/rafikid — see its doc comment.
func TestRecoveryScopingLeavesAnotherDaemonsChildAlone(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	idA := nextDaemonID()
	dA := bootDaemonDB(t, idA)
	childID := dA.spawnChild(t)

	pool := openPool(t, dsn)
	ctx := context.Background()

	// A row in flight inside A's live child.
	const inboxID = "ibx-recovery-scoping"
	if _, err := pool.Exec(ctx,
		`DELETE FROM conversations.agent_inbox WHERE id = $1`, inboxID); err != nil {
		t.Fatalf("clear stale inbox row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.agent_inbox (id, child_id, mode, body, state)
		VALUES ($1, $2, 'prompt', 'in flight on daemon A', 'sent')`,
		inboxID, childID); err != nil {
		t.Fatalf("seed inbox row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.agent_inbox WHERE id = $1`, inboxID)
	})

	// B boots and recovers. Waiting until B answers for the child proves B's
	// loadChildren actually processed that row — a fixed sleep would pass
	// before recovery had run at all.
	dB := bootDaemonDB(t, nextDaemonID())
	waitUntilKnown(t, dB, childID)

	var state string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT state FROM conversations.agent_inbox WHERE id = $1`, inboxID).Scan(&state), "read inbox state")
	c.Require().Eq("sent", state, "daemon B reset another daemon's in-flight inbox row to")

	// The row still belongs to A. B must not have stamped itself onto it.
	var owner string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(daemon_id,'') FROM conversations.child WHERE child_id = $1`,
		childID).Scan(&owner), "read daemon_id")
	c.Eq(idA, owner, "child %s changed owner to %q; want it left with", childID, owner)
}

// TestRecoveryScopingSurfacesTheOwningDaemon pins design §4.4's claim that
// ownership is ALREADY visible and needs no new wire field: the rafiki/daemon
// label is stamped by the owner (controller.go:2960), survives
// SessionFromRecord (record.go:232), and reaches protocol.ChildSummary.Labels.
//
// If this fails, §4.4 is wrong and the chunk owes a real wire field.
func TestRecoveryScopingSurfacesTheOwningDaemon(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	idA := nextDaemonID()
	dA := bootDaemonDB(t, idA)
	childID := dA.spawnChild(t)

	dB := bootDaemonDB(t, nextDaemonID())
	waitUntilKnown(t, dB, childID)

	summary := getChildSummary(t, dB, childID)
	got := summary.GetLabels()["rafiki/daemon"]
	assert.NewCollecting(t).Eq(idA, got, "rafiki/daemon label = %q, want %q — ownership must be visible "+
		"through daemon B with no new wire field", got, idA)
}

// waitUntilKnown blocks until d answers GetChild for childID, proving d's
// recovery walked that row.
func waitUntilKnown(t *testing.T, d *daemon, childID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := d.control(t).GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		cancel()
		if err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("daemon never reported child %s; recovery did not process the row", childID)
}

// getChildSummary reads one child over Connect.
func getChildSummary(t *testing.T, d *daemon, childID string) *rafikiv1.ChildSummary {
	t.Helper()
	return getChild(t, d.control(t), childID)
}
