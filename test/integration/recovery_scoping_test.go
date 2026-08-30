// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
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
	if err := pool.QueryRow(ctx,
		`SELECT state FROM conversations.agent_inbox WHERE id = $1`, inboxID).Scan(&state); err != nil {
		t.Fatalf("read inbox state: %v", err)
	}
	if state != "sent" {
		t.Fatalf("daemon B reset another daemon's in-flight inbox row to %q; want %q", state, "sent")
	}

	// The row still belongs to A. B must not have stamped itself onto it.
	var owner string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(daemon_id,'') FROM conversations.child WHERE child_id = $1`,
		childID).Scan(&owner); err != nil {
		t.Fatalf("read daemon_id: %v", err)
	}
	if owner != idA {
		t.Errorf("child %s changed owner to %q; want it left with %q", childID, owner, idA)
	}
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
	if got := summary.Labels["rafiki/daemon"]; got != idA {
		t.Errorf("rafiki/daemon label = %q, want %q — ownership must be visible "+
			"through daemon B with no new wire field", got, idA)
	}
}

// waitUntilKnown blocks until d answers ctrl_get for childID, proving d's
// recovery walked that row.
func waitUntilKnown(t *testing.T, d *daemon, childID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw := d.request(t, fmt.Sprintf(
			`{"type":"ctrl_get","id":"wait","childId":%q}`, childID))
		var r protocol.Response
		mustUnmarshal(t, raw, &r)
		if r.Success {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("daemon never reported child %s; recovery did not process the row", childID)
}

// getChildSummary reads one child through ctrl_get.
func getChildSummary(t *testing.T, d *daemon, childID string) protocol.ChildSummary {
	t.Helper()
	raw := d.request(t, fmt.Sprintf(
		`{"type":"ctrl_get","id":"g1","childId":%q}`, childID))
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_get failed: %+v", r.Error)
	}
	var summary protocol.ChildSummary
	mustUnmarshal(t, r.Data, &summary)
	return summary
}
