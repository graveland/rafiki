// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// failingChildStore is a childstore.ChildStore whose Upsert always fails,
// recording the child ids it was asked to write. failIDs, when non-nil, narrows
// the failure to those ids so a test can mix a failing child with a healthy one.
type failingChildStore struct {
	stubChildStore
	failIDs map[string]bool
	failed  []string
}

func (f *failingChildStore) Upsert(_ context.Context, rec childstore.ChildRecord) error {
	f.failed = append(f.failed, rec.ChildID)
	if f.failIDs == nil || f.failIDs[rec.ChildID] {
		return errors.New("child store upsert failed")
	}
	return nil
}

// Close must persist the child's current snapshot BEFORE it forgets the child:
// the upsert carries session_id to conversations.child, and the tombstone (the
// store's Delete) stamps closed_at afterwards. A row written before the session
// id was known must still end up carrying it.
//
// Fails against the pre-change Close, which never called writeRecord: the row's
// session_id stays empty.
func TestClosePersistsSessionIDBeforeTombstone(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	ctrl := newTestController(t)
	ctrl.pool = pool
	ctrl.children = childstoredb.New(pool)

	const id = "c_close_persist_sid"
	ctx := context.Background()
	var convID string
	ck.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by) VALUES ('test', 'server') RETURNING id::text`).Scan(&convID),
		"seed a conversation for the fundi child's session id")

	ctrl.st.Insert(&childstore.Session{
		ChildID: id, Kind: protocol.KindFundi, Status: protocol.StatusExited,
		Cwd: t.TempDir(), StartedAt: time.Now(), LastActivity: time.Now(),
	})
	ck.Require().NoError(ctrl.writeRecord(id), "initial persist (without a session id)")
	ck.Require().NoError(ctrl.st.Update(id, func(s *childstore.Session) {
		s.SessionID = convID
	}), "set session id")

	ck.Require().NoError(ctrl.Close(id), "Close")

	var gotSession string
	var closedAt *time.Time
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT COALESCE(session_id::text, ''), closed_at FROM conversations.child WHERE child_id = $1`,
		id).Scan(&gotSession, &closedAt), "read the closed child's row")
	ck.Eq(convID, gotSession,
		"Close must persist the child's session id before the tombstone, not forget the child with an empty row")
	ck.NotNil(closedAt, "the row must be tombstoned (closed_at set)")
}

// A child whose lineage row cannot be written must FAIL CLOSED: Close returns an
// error and leaves the child in the live store rather than forgetting it with
// its spend missing from its ancestors' subtree.
//
// Fails against the pre-change Close, which ignored the writeRecord error,
// returned nil, and forgot the child.
func TestCloseFailsClosedWhenPersistFails(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.children = &failingChildStore{}

	const id = "c_close_persist_fail"
	ctrl.st.Insert(&childstore.Session{
		ChildID: id, Kind: protocol.KindFundi, Status: protocol.StatusExited,
		Cwd: t.TempDir(), StartedAt: time.Now(),
	})

	err := ctrl.Close(id)
	ck.Require().Error(err, "Close must fail closed when the child's row cannot be persisted")
	ck.StrContains(err.Error(), id, "the refusal must name the child: %v", err)
	_, ok := ctrl.st.Get(id)
	ck.True(ok, "a child whose lineage was not recorded must stay in the live store, not be forgotten")
}

// CloseAllExited must persist each child before forgetting it and SKIP the ones
// it cannot: the healthy children still close, the failing one stays, and the
// failure is surfaced in the returned error.
//
// Fails against the pre-change CloseAllExited, which forgot both children and
// returned a nil error.
func TestCloseAllExitedSkipsFailedChildAndClosesTheRest(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const bad, good = "c_closeall_bad", "c_closeall_good"
	ctrl.children = &failingChildStore{failIDs: map[string]bool{bad: true}}
	now := time.Now()
	for _, id := range []string{bad, good} {
		ctrl.st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindFundi, Status: protocol.StatusExited,
			Cwd: t.TempDir(), StartedAt: now, ExitedAt: now,
		})
	}

	closed, err := ctrl.CloseAllExited(0)
	ck.Require().Error(err, "a child that could not be persisted must be surfaced in the result")
	ck.StrContains(err.Error(), "1", "the error must name the failure count: %v", err)
	ck.ElementsMatch([]string{good}, closed, "only the healthy child is closed; got %v", closed)

	if _, ok := ctrl.st.Get(bad); !ok {
		t.Fatal("the child whose row could not be persisted was wrongly forgotten")
	}
	if _, ok := ctrl.st.Get(good); ok {
		t.Fatal("the healthy exited child survived CloseAllExited")
	}
}

// A synthetic thread child has no durable row: Close must take its unchanged
// store-delete path and never touch the (failing) persistence store.
func TestCloseNativeChildDoesNotPersist(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	// Any writeRecord would fail; the native path must not attempt one.
	ctrl.children = &failingChildStore{}

	id := insertNativeParent(t, ctrl, "c_native_close_parent", protocol.StatusIdle)
	ck.Require().NoError(ctrl.st.Update(id, func(s *childstore.Session) {
		s.Status = protocol.StatusExited
	}), "mark the native child exited")
	ck.Require().NoError(ctrl.Close(id), "closing a native child must not touch the durable store")
	_, ok := ctrl.st.Get(id)
	ck.False(ok, "a native child's store delete IS its close")
}
