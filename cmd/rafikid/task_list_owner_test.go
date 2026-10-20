// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// TestListTasksOwnerClaudeChildWithoutLedgerCreatesNoConversation pins the
// read path: a claude child whose conversation the proxy has not captured yet
// answers an empty ledger, and answering it does not create the conversation
// row ensureLedgerConversation would — reading must never write.
func TestListTasksOwnerClaudeChildWithoutLedgerCreatesNoConversation(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := context.Background()
	// Unique per run: the scratch database is migrated but shared with no
	// other test, and the child id keeps the count below unambiguous anyway.
	childID := fmt.Sprintf("c_owner_%d", time.Now().UnixNano())

	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: childID, Kind: protocol.KindClaude, Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	ledger := tasks.NewMemoryStore()
	if _, err := ledger.Add(ctx, "conv-unrelated", "", []tasks.NewTask{{Content: "someone else's row"}}); err != nil {
		t.Fatalf("seed an unrelated ledger: %v", err)
	}
	c := &Controller{
		st: st, pool: pool, captureStore: capture.NewCaptureStore(pool), tasks: ledger,
	}

	rows, err := c.TaskList(ctx, protocol.TaskListRequest{OwnerChildID: childID})
	ck.Require().NoError(err, "TaskList for a child with no ledger yet")
	ck.Len(rows, 0, "a child with no ledger answers an empty ledger, not the whole store")

	var n int
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1`, childID).Scan(&n),
		"count conversations for the child")
	ck.Eq(0, n, "reading a ledger must not create the child's conversation row")
}

// TestListTasksOwnerClaudeChildWithLedgerReturnsItsRows pins the lookup half:
// the ledger of a proxy-captured child is found by the same external_ref row
// the proxy keys its traffic to, and the rows come back keyed by it.
func TestListTasksOwnerClaudeChildWithLedgerReturnsItsRows(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := context.Background()
	childID := fmt.Sprintf("c_owner_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cctx, `DELETE FROM conversations.conversation WHERE external_ref = $1`, childID); err != nil {
			t.Logf("cleanup owner conversation: %v", err)
		}
	})

	captureStore := capture.NewCaptureStore(pool)
	convID, err := captureStore.EnsureConversationByExternalRef(ctx, capture.ConversationRef{
		ExternalRef:      childID,
		DrivenBy:         string(store.DrivenByClient),
		OriginEntrypoint: string(protocol.KindClaude),
	})
	ck.Require().NoError(err, "seed the captured conversation")

	ledger := tasks.NewMemoryStore()
	_, err = ledger.Add(ctx, convID, "", []tasks.NewTask{
		{Content: "fix the bug", ActiveForm: "fixing the bug"},
	})
	ck.Require().NoError(err, "seed the child's ledger")

	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: childID, Kind: protocol.KindClaude, Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	c := &Controller{st: st, pool: pool, captureStore: captureStore, tasks: ledger}

	rows, err := c.TaskList(ctx, protocol.TaskListRequest{OwnerChildID: childID})
	ck.Require().NoError(err, "TaskList")
	ck.Len(rows, 1, "the child's own ledger")
	if len(rows) == 1 {
		ck.Eq("fix the bug", rows[0].Content, "content")
		ck.Eq(convID, rows[0].ConversationID, "the row is keyed by the captured conversation")
	}

	var n int
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1`, childID).Scan(&n),
		"count conversations for the child")
	ck.Eq(1, n, "the lookup reuses the captured conversation rather than creating a second one")
}

// TestListTasksOwnerFundiChildUsesItsSessionID pins the engine-minted half: a
// fundi child's ledger is keyed by the conversation uuid its engine minted,
// resolved without a database round trip.
func TestListTasksOwnerFundiChildUsesItsSessionID(t *testing.T) {
	ck := assert.NewAborting(t)
	const owner = "c_fundi_owner"
	ledger := tasks.NewMemoryStore()
	if _, err := ledger.Add(t.Context(), "sess-owner", "", []tasks.NewTask{{Content: "delegate it"}}); err != nil {
		t.Fatalf("seed the engine's ledger: %v", err)
	}
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: owner, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		StartedAt: time.Now(), SessionID: "sess-owner",
	})
	c := &Controller{st: st, tasks: ledger}

	rows, err := c.TaskList(t.Context(), protocol.TaskListRequest{OwnerChildID: owner})
	ck.Require().NoError(err, "TaskList")
	ck.Len(rows, 1, "the fundi child's ledger is keyed by its engine's conversation")
	if len(rows) == 1 {
		ck.Eq("delegate it", rows[0].Content, "content")
	}
}

// TestListTasksOwnerUnknownChildIsEmpty pins the unknown-id arm: an operator
// naming a child the daemon does not know answers an empty ledger, not the
// whole store and not an error. The unrelated row is the assertion's teeth —
// without the early return the empty conversation filter matches everything.
func TestListTasksOwnerUnknownChildIsEmpty(t *testing.T) {
	ck := assert.NewAborting(t)
	ledger := tasks.NewMemoryStore()
	if _, err := ledger.Add(t.Context(), "conv-other", "", []tasks.NewTask{{Content: "unrelated"}}); err != nil {
		t.Fatalf("seed an unrelated ledger: %v", err)
	}
	c := &Controller{st: childstore.New(), tasks: ledger}

	rows, err := c.TaskList(t.Context(), protocol.TaskListRequest{OwnerChildID: "c_nobody"})
	ck.Require().NoError(err, "an unknown owner child is an empty ledger")
	ck.Len(rows, 0, "an unknown owner child answers no rows")
}
