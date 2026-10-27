// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// A close must ANNOUNCE itself, before the row is gone, carrying the lineage a
// subscriber that is still drawing the row needs: the scope predicate resolves
// "is this child in my subtree?" against the childstore, which a close is about
// to empty. Without the chain, the one client that still shows the row -- a
// cockpit attached to an ancestor -- could never be told it is gone, and the
// event would only reach a bare all-scope attach.
func TestClosePublishesChildClosedWithItsLineage(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	for _, c := range []struct{ id, parent string }{
		{"c_evt_root", ""},
		{"c_evt_mid", "c_evt_root"},
		{"c_evt_leaf", "c_evt_mid"},
	} {
		labels := map[string]string{"owner": "brent"}
		if c.parent != "" {
			labels[childstore.LabelParent] = c.parent
			labels[childstore.LabelRoot] = "c_evt_root"
		}
		ctrl.st.Insert(&childstore.Session{
			ChildID: c.id, Kind: protocol.KindFundi, Status: protocol.StatusExited,
			Labels: labels, Cwd: t.TempDir(), StartedAt: time.Now(), LastActivity: time.Now(),
		})
	}

	live, cancel := ctrl.native.Subscribe("c_evt_leaf")
	defer cancel()

	ck.Require().NoError(ctrl.Close("c_evt_leaf"), "Close")

	var ev *rafikiv1.Event
	select {
	case ev = <-live:
	case <-time.After(2 * time.Second):
		t.Fatal("close published nothing: a subscriber still drawing the row would never learn it is gone")
	}
	cc := ev.GetChildClosed()
	ck.Require().NotNil(cc, "payload = %T, want ChildClosed", ev.GetPayload())
	ck.Eq("c_evt_leaf", ev.GetChildId(), "the envelope names the closed child")
	ck.EqDeep([]string{"c_evt_mid", "c_evt_root"}, cc.GetAncestors(),
		"ancestors must be parent-first up to the root: a mid-tree subscriber is the case this exists for")
	ck.Eq("brent", cc.GetLabels()["owner"], "the child's labels ride the event so a selector can still be evaluated")
	ck.Eq("c_evt_mid", cc.GetLabels()[childstore.LabelParent], "the parent label")

	_, still := ctrl.st.Get("c_evt_leaf")
	ck.False(still, "the event must describe the close, not replace it: the row is gone")
}

// A top-level child has no chain, and must still be announced.
func TestClosePublishesForATopLevelChild(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_evt_alone", Kind: protocol.KindFundi, Status: protocol.StatusExited,
		Labels: map[string]string{}, Cwd: t.TempDir(), StartedAt: time.Now(), LastActivity: time.Now(),
	})

	live, cancel := ctrl.native.Subscribe("c_evt_alone")
	defer cancel()

	ck.Require().NoError(ctrl.Close("c_evt_alone"), "Close")

	select {
	case ev := <-live:
		ck.Require().NotNil(ev.GetChildClosed(), "payload = %T, want ChildClosed", ev.GetPayload())
		ck.Eq(0, len(ev.GetChildClosed().GetAncestors()), "a top-level child has no ancestors")
	case <-time.After(2 * time.Second):
		t.Fatal("a top-level close published nothing")
	}
}

// Every close site announces, not just Close: a cascading close of a parent's
// synthetic thread children is a close too, and their rows are on the rail.
func TestCloseNativeChildrenOfPublishesChildClosed(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	for _, id := range []string{"c_evt_thread_a", "c_evt_thread_b"} {
		ctrl.st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindFundi, Status: protocol.StatusExited,
			Native: true, Labels: map[string]string{childstore.LabelParent: "c_evt_parent"},
			Cwd: t.TempDir(), StartedAt: time.Now(), LastActivity: time.Now(),
		})
	}

	live, cancel := ctrl.native.SubscribeAll()
	defer cancel()

	closed := ctrl.closeNativeChildrenOf("c_evt_parent")
	ck.Require().Eq(2, len(closed), "closed native children")

	seen := map[string]bool{}
	for range 2 {
		select {
		case ev := <-live:
			if ev.GetChildClosed() == nil {
				continue
			}
			seen[ev.GetChildId()] = true
			ck.EqDeep([]string{"c_evt_parent"}, ev.GetChildClosed().GetAncestors(), "ancestors of %s", ev.GetChildId())
		case <-time.After(2 * time.Second):
			t.Fatal("a native thread child closed with no child_closed event")
		}
	}
	ck.True(seen["c_evt_thread_a"] && seen["c_evt_thread_b"], "seen = %v", seen)
}
