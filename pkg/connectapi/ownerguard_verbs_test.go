// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// This file pins the owner guard on the lifecycle/inbox verbs: a non-admin
// user's foreign id is refused identically to an unknown one, BEFORE the
// backend is touched, and an owned id reaches it.

// verbsRecordingLister records the calls GetChild/ListChildren make, so a
// refusal can be proven to have never reached the lister.
type verbsRecordingLister struct {
	all       []protocol.ChildSummary
	listCalls int
	getCalls  []string
}

func (f *verbsRecordingLister) ListChildren([]string) []protocol.ChildSummary {
	f.listCalls++
	return f.all
}

func (f *verbsRecordingLister) GetChild(childID string) (protocol.ChildSummary, bool) {
	f.getCalls = append(f.getCalls, childID)
	for _, c := range f.all {
		if c.ChildID == childID {
			return c, true
		}
	}
	return protocol.ChildSummary{}, false
}

// verbsFixture is one Server with every backend these verbs use recorded, and
// a guard that owns exactly "c_mine".
type verbsFixture struct {
	s       *connectapi.Server
	guard   *fakeOwnerGuard
	inbox   *fakeAccepter
	lc      *fakeLifecycle
	lister  *verbsRecordingLister
	history *recordingLoader
}

func newVerbsFixture(t *testing.T) *verbsFixture {
	t.Helper()
	f := &verbsFixture{
		guard:   &fakeOwnerGuard{owned: map[string]bool{"c_mine": true}, convs: map[string]bool{}},
		inbox:   &fakeAccepter{},
		lc:      &fakeLifecycle{},
		lister:  &verbsRecordingLister{all: []protocol.ChildSummary{{ChildID: "c_mine"}, {ChildID: "c_foreign"}}},
		history: &recordingLoader{},
	}
	f.s = connectapi.NewServer(f.history)
	f.s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return f.guard })
	f.s.SetInbox(f.inbox)
	f.s.SetChildLifecycle(f.lc)
	f.s.SetChildLister(f.lister)
	f.s.SetChildResolver(fakeResolver{})
	return f
}

// guardedVerb is one guarded verb: how to invoke it for a child id, and how
// to tell whether its backend was reached.
type guardedVerb struct {
	name    string
	invoke  func(f *verbsFixture, childID string) error
	reached func(f *verbsFixture) bool
}

// guardedVerbs is the per-verb table every owner-guard test below walks.
func guardedVerbs() []guardedVerb {
	return []guardedVerb{
		{"Send", func(f *verbsFixture, id string) error {
			_, err := f.s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: id, Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("hi"),
			}))
			return err
		}, func(f *verbsFixture) bool { return f.inbox.got.ChildID != "" }},
		{"Kill", func(f *verbsFixture, id string) error {
			_, err := f.s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{ChildId: id}))
			return err
		}, func(f *verbsFixture) bool { return f.lc.killedID != "" }},
		{"Close", func(f *verbsFixture, id string) error {
			_, err := f.s.Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{ChildId: id}))
			return err
		}, func(f *verbsFixture) bool { return f.lc.closedID != "" }},
		{"GetChild", func(f *verbsFixture, id string) error {
			_, err := f.s.GetChild(context.Background(), connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: id}))
			return err
		}, func(f *verbsFixture) bool { return len(f.lister.getCalls) > 0 }},
		{"GetHistory", func(f *verbsFixture, id string) error {
			_, err := f.s.GetHistory(context.Background(), connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: id}))
			return err
		}, func(f *verbsFixture) bool { return f.history.gotID != "" }},
		{"SetBudget", func(f *verbsFixture, id string) error {
			_, err := f.s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: id, MaxCost: 1}))
			return err
		}, func(f *verbsFixture) bool { return f.lc.budgetChildID != "" }},
		{"SetRouting", func(f *verbsFixture, id string) error {
			_, err := f.s.SetRouting(context.Background(), connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: id, Delta: "prefer=x"}))
			return err
		}, func(f *verbsFixture) bool { return f.lc.routingChildID != "" }},
		{"Rename", func(f *verbsFixture, id string) error {
			_, err := f.s.Rename(context.Background(), connect.NewRequest(&rafikiv1.RenameRequest{ChildId: id, Name: "renamed"}))
			return err
		}, func(f *verbsFixture) bool { return f.lc.renameChildID != "" }},
	}
}

// TestOwnerGuardRefusesForeignAndUnknownIdentically is the anti-probe pin: for
// every guarded verb, a foreign id and an unknown id answer the same
// PermissionDenied text (substituting only the id) and the backend is never
// reached — not the lifecycle, not the inbox, not the lister, not the store.
func TestOwnerGuardRefusesForeignAndUnknownIdentically(t *testing.T) {
	c := assert.NewAborting(t)
	var firstText func(id string) string

	for _, verb := range guardedVerbs() {
		for _, id := range []string{"c_foreign", "c_unknown"} {
			f := newVerbsFixture(t)
			err := verb.invoke(f, id)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s(%s) = %v, want PermissionDenied", verb.name, id, err)
			c.False(verb.reached(f), "%s(%s) reached its backend despite the owner refusal", verb.name, id)

			msg := err.Error()
			if firstText == nil {
				firstText = func(id string) string { return strings.ReplaceAll(msg, "c_foreign", id) }
				c.False(!strings.Contains(msg, "is not owned by you"), "%s refusal %q does not carry the owner text", verb.name, msg)
				continue
			}
			c.Eq(firstText(id), msg, "%s(%s) refusal text differs from the first refusal; foreign and unknown ids must be indistinguishable", verb.name, id)
		}
	}
}

// TestOwnerGuardAdmitsOwnedChild is the control group: the same verbs reach
// their backend for an id the caller owns, so the guard refuses only foreign
// children and never the owner's own.
func TestOwnerGuardAdmitsOwnedChild(t *testing.T) {
	c := assert.NewAborting(t)
	for _, verb := range guardedVerbs() {
		f := newVerbsFixture(t)
		err := verb.invoke(f, "c_mine")
		c.NoError(err, "%s(c_mine) = %v, want the handler to reach its backend", verb.name, err)
		c.True(verb.reached(f), "%s(c_mine) never reached its backend", verb.name)
	}
}

// TestOwnerGuardEmptyIDStaysInvalidArgument pins the one shape that does NOT
// go through the guard: a missing required child_id. Every verb has always
// answered it InvalidArgument, it names no child (so it probes nothing), and
// making it credential-dependent — PermissionDenied for a guarded caller,
// InvalidArgument for the operator — would itself disclose which kind of
// credential is asking.
func TestOwnerGuardEmptyIDStaysInvalidArgument(t *testing.T) {
	c := assert.NewAborting(t)
	for _, verb := range guardedVerbs() {
		f := newVerbsFixture(t)
		err := verb.invoke(f, "")
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err),
			"%s with an empty child_id = %v, want InvalidArgument for every caller", verb.name, err)
		c.False(verb.reached(f), "%s with an empty child_id reached its backend", verb.name)
	}
}

// TestOwnerGuardSpawnRefusesForeignParent pins the descendant-owner invariant:
// a non-admin user may parent a spawn only under a child it owns, because the
// new child is stamped with the caller's owner. A top-level spawn names no
// parent and is admitted.
func TestOwnerGuardSpawnRefusesForeignParent(t *testing.T) {
	c := assert.NewAborting(t)

	f := newVerbsFixture(t)
	_, err := f.s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp", Name: "w", ParentChildId: "c_foreign",
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Spawn under a foreign parent = %v, want PermissionDenied", err)
	c.Eq("", f.lc.got.Cwd, "Spawn reached the lifecycle despite the foreign parent: %+v", f.lc.got)

	for _, parent := range []string{"", "c_mine"} {
		f := newVerbsFixture(t)
		_, err := f.s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd: "/tmp", Name: "w", ParentChildId: parent,
		}))
		c.NoError(err, "Spawn with parent %q = %v, want admitted", parent, err)
		c.Eq("/tmp", f.lc.got.Cwd, "Spawn with parent %q never reached the lifecycle", parent)
		c.Eq(parent, f.lc.got.ParentChildID, "ParentChildID for parent %q", parent)
	}
}

// TestOwnerGuardListChildrenFilters pins the filtered verb: the operator
// branch returns exactly the owned subset when a guard is wired, and the whole
// fleet when the source resolves nil (the admin/UDS shape).
func TestOwnerGuardListChildrenFilters(t *testing.T) {
	c := assert.NewAborting(t)

	f := newVerbsFixture(t)
	resp, err := f.s.ListChildren(context.Background(), connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	c.NoError(err, "ListChildren")
	c.Len(resp.Msg.GetChildren(), 1, "got %d children, want only the owned one", len(resp.Msg.GetChildren()))
	c.Eq("c_mine", resp.Msg.GetChildren()[0].GetChildId(), "surviving child")

	s := connectapi.NewServer(nil)
	s.SetChildLister(&verbsRecordingLister{all: []protocol.ChildSummary{{ChildID: "c_mine"}, {ChildID: "c_foreign"}}})
	s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return nil })
	resp, err = s.ListChildren(context.Background(), connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	c.NoError(err, "ListChildren (unrestricted)")
	c.Len(resp.Msg.GetChildren(), 2, "got %d children, want the whole fleet", len(resp.Msg.GetChildren()))
}
