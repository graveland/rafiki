// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"
)

// The wire-level proof of wave 1's childScoped policy: a real per-child
// credential, resolved by a real UserTokenAuth through the same
// connectControlRoute composition the daemon mounts, acting on a real
// childstore tree. The verb MECHANICS are recording fakes, so "allowed" is
// observed positively (the verb reached its work) and "denied" is exactly
// CodePermissionDenied with the work untouched — an absence-of-error alone
// could not tell an authorized call from a silently skipped one.
const childCallerToken = "rfk_per_child_secret_c_mine"

// childTree builds the tree every childScoped test addresses:
//
//	c_root
//	 ├── c_mine      (the caller, owner u1)
//	 │    └── c_kid
//	 │         └── c_grand
//	 └── c_sib       (sibling of the caller)
//	c_stranger       (another top-level tree)
//
// Every child is fundi-kind with SessionID "conv-<childID>", so the
// conversation-keyed reads (GetHistory, ListTasks) resolve through the real
// Controller mapping without a database.
func childTree(t *testing.T) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	insert := func(id, parent, root string) {
		labels := map[string]string{}
		if parent != "" {
			labels[childstore.LabelParent] = parent
			labels[childstore.LabelRoot] = root
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Kind: protocol.KindFundi,
			SessionID: "conv-" + id, StartedAt: time.Now(), Labels: labels,
		})
	}
	insert("c_root", "", "")
	insert("c_mine", "c_root", "c_root")
	insert("c_kid", "c_mine", "c_root")
	insert("c_grand", "c_kid", "c_root")
	insert("c_sib", "c_root", "c_root")
	insert("c_stranger", "", "")
	return c
}

// recordingLifecycle stands in for the daemon's spawn/kill/close mechanics so
// the matrix can prove an authorized verb REACHED them.
type recordingLifecycle struct {
	spawns []connectapi.SpawnParams
	kills  []string
	closes []string
}

func (l *recordingLifecycle) Spawn(_ context.Context, p connectapi.SpawnParams) (string, error) {
	l.spawns = append(l.spawns, p)
	return "c_new", nil
}

func (l *recordingLifecycle) Kill(_ context.Context, childID string, _, _ int64) (connectapi.KillOutcome, error) {
	l.kills = append(l.kills, childID)
	return connectapi.KillOutcome{}, nil
}

func (l *recordingLifecycle) Close(_ context.Context, childID string) error {
	l.closes = append(l.closes, childID)
	return nil
}

func (l *recordingLifecycle) SetBudget(_ context.Context, _ string, _ float64) error {
	return nil
}

type recordingInbox struct{ kids []string }

func (i *recordingInbox) Accept(_ context.Context, in inbox.Inbound) (string, error) {
	i.kids = append(i.kids, in.ChildID)
	return "m1", nil
}

type recordingHistory struct{ loads []string }

func (h *recordingHistory) Load(_ context.Context, conversationID string) ([]store.Message, error) {
	h.loads = append(h.loads, conversationID)
	return nil, nil
}

type recordingTaskLister struct{ convs []string }

func (t *recordingTaskLister) TaskList(_ context.Context, req protocol.TaskListRequest) ([]tasks.Task, error) {
	t.convs = append(t.convs, req.ConversationID)
	return nil, nil
}

// receiveToEnd consumes one server-streaming StreamEvents response and folds
// its clean end (EOF, or the stream ending because no live source is wired)
// into nil, so callers see exactly the RPC error: refused streams surface the
// header code, authorized ones surface nil.
func receiveToEnd(stream *connect.ServerStreamForClient[rafikiv1.Event]) error {
	for stream.Receive() {
	}
	return stream.Err()
}

// childScopeFixture mounts the full stack: auth middleware → policy gate →
// handlers, with the REAL childScopeFor over childTree and recording fakes
// behind every childScoped verb. Only the per-child credential for c_mine is
// wired; the other child shapes are gate-refused before any handler runs.
type childScopeFixture struct {
	ctrl      *Controller
	client    rafikiv1connect.ControlClient
	lifecycle *recordingLifecycle
	inbox     *recordingInbox
	history   *recordingHistory
	tasks     *recordingTaskLister
}

func mountChildScope(t *testing.T) *childScopeFixture {
	t.Helper()

	ctrl := childTree(t)
	fx := &childScopeFixture{
		ctrl:      ctrl,
		lifecycle: &recordingLifecycle{},
		inbox:     &recordingInbox{},
		history:   &recordingHistory{},
		tasks:     &recordingTaskLister{},
	}

	auth := server.NewUserTokenAuth(
		fakeUserStore{token: proxyUserToken, id: users.Identity{UserID: "u1", Username: "brent"}},
		proxyBootToken,
		server.DefaultAuthCacheTTL,
	)
	auth.SetChildTokenLookup(func(token string) (childID, ownerUserID string, ok bool) {
		if token == childCallerToken {
			return "c_mine", "u1", true
		}
		return "", "", false
	})
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_mine" {
			return "u1", true
		}
		return "", false
	})

	srv := connectapi.NewServer(fx.history)
	srv.SetChildScopeSource(ctrl.childScopeFor)
	srv.SetChildLister(ctrl)
	srv.SetChildResolver(ctrl)
	srv.SetLineage(ctrl)
	srv.SetInbox(fx.inbox)
	srv.SetTaskLister(fx.tasks)
	srv.SetChildLifecycle(fx.lifecycle)

	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv)
	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	fx.client = rafikiv1connect.NewControlClient(ts.Client(), ts.URL)
	return fx
}

// childCaller is the Authorization header the per-child credential sends.
func childCaller(h http.Header) { h.Set("Authorization", "Bearer "+childCallerToken) }

// userCaller is the control credential: the operator path must keep passing
// parent_child_id through and seeing the whole fleet.
func userCaller(h http.Header) { h.Set("Authorization", "Bearer "+proxyUserToken) }

// gateRefusedCredentials are the two child shapes that name no child with
// authority; the gate refuses them before any handler runs.
func gateRefusedCredentials() []struct {
	name string
	set  func(h http.Header)
} {
	return []struct {
		name string
		set  func(h http.Header)
	}{
		{"per-boot+session", func(h http.Header) {
			h.Set("Authorization", "Bearer "+proxyBootToken)
			h.Set("X-Rafiki-Session", "c_mine")
		}},
		{"per-boot bare", func(h http.Header) {
			h.Set("Authorization", "Bearer "+proxyBootToken)
		}},
	}
}

// TestConnectChildScopedVerbs runs the brief's per-verb matrix: a child
// credential acting on its descendant (allowed, and reaching the verb's
// mechanics), its grand-descendant (same), itself, a sibling, its parent, a
// stranger tree, and an unknown id (all PermissionDenied, mechanics
// untouched).
func TestConnectChildScopedVerbs(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	targets := []struct {
		name string
		id   string
		want bool
	}{
		{"descendant", "c_kid", true},
		{"grand-descendant", "c_grand", true},
		{"self", "c_mine", false},
		{"sibling", "c_sib", false},
		{"parent", "c_root", false},
		{"other tree", "c_stranger", false},
		{"unknown", "c_nonexistent", false},
	}

	verbs := []struct {
		name    string
		call    func(target string, set func(h http.Header)) error
		touched func() bool
	}{
		{"GetChild", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: target})
			set(req.Header())
			_, err := fx.client.GetChild(ctx, req)
			return err
		}, func() bool { return false }},
		{"GetHistory", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: target})
			set(req.Header())
			_, err := fx.client.GetHistory(ctx, req)
			return err
		}, func() bool { return len(fx.history.loads) > 0 }},
		{"Send", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: target, Mode: rafikiv1.SendMode_SEND_MODE_PROMPT,
				Blocks: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}}}},
			})
			set(req.Header())
			_, err := fx.client.Send(ctx, req)
			return err
		}, func() bool { return len(fx.inbox.kids) > 0 }},
		{"Kill", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.KillRequest{ChildId: target})
			set(req.Header())
			_, err := fx.client.Kill(ctx, req)
			return err
		}, func() bool { return len(fx.lifecycle.kills) > 0 }},
		{"Close", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.CloseRequest{ChildId: target})
			set(req.Header())
			_, err := fx.client.Close(ctx, req)
			return err
		}, func() bool { return len(fx.lifecycle.closes) > 0 }},
		{"StreamEvents/child-subject", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.StreamEventsRequest{
				Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: target}},
			})
			set(req.Header())
			stream, err := fx.client.StreamEvents(ctx, req)
			if err != nil {
				return err
			}
			return receiveToEnd(stream)
		}, func() bool { return false }},
		{"StreamEvents/subtree-subject", func(target string, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.StreamEventsRequest{
				Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Subtree{Subtree: target}},
			})
			set(req.Header())
			stream, err := fx.client.StreamEvents(ctx, req)
			if err != nil {
				return err
			}
			return receiveToEnd(stream)
		}, func() bool { return false }},
	}

	for _, verb := range verbs {
		t.Run(verb.name, func(t *testing.T) {
			for _, tgt := range targets {
				t.Run(tgt.name, func(t *testing.T) {
					err := verb.call(tgt.id, childCaller)
					if tgt.want {
						if connect.CodeOf(err) == connect.CodePermissionDenied {
							t.Fatalf("%s on %s = %v, want allowed", verb.name, tgt.id, err)
						}
						return
					}
					if connect.CodeOf(err) != connect.CodePermissionDenied {
						t.Fatalf("%s on %s = %v, want %v", verb.name, tgt.id, err, connect.CodePermissionDenied)
					}
				})
			}
		})
	}
}

// TestConnectChildScopedMechanicsReached pins the positive direction per verb
// with the recording fakes: an authorized call actually reached its verb's
// work, and a denied one never did. (The matrix above asserts codes; this one
// asserts that the codes are not masking a silently skipped handler.)
func TestConnectChildScopedMechanicsReached(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	// Send to a descendant: accepted by the inbox.
	req := connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_kid", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}}}},
	})
	childCaller(req.Header())
	if _, err := fx.client.Send(ctx, req); err != nil {
		t.Fatalf("Send to descendant: %v", err)
	}
	if len(fx.inbox.kids) != 1 || fx.inbox.kids[0] != "c_kid" {
		t.Fatalf("inbox saw %v, want exactly c_kid", fx.inbox.kids)
	}

	// Kill a descendant: reached the lifecycle.
	kreq := connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_grand"})
	childCaller(kreq.Header())
	if _, err := fx.client.Kill(ctx, kreq); err != nil {
		t.Fatalf("Kill of grand-descendant: %v", err)
	}
	if len(fx.lifecycle.kills) != 1 || fx.lifecycle.kills[0] != "c_grand" {
		t.Fatalf("lifecycle saw %v, want exactly c_grand", fx.lifecycle.kills)
	}

	// A refused kill never reaches the lifecycle.
	sreq := connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_sib"})
	childCaller(sreq.Header())
	if _, err := fx.client.Kill(ctx, sreq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Kill of sibling = %v, want PermissionDenied", err)
	}
	if len(fx.lifecycle.kills) != 1 {
		t.Fatalf("refused kill reached the lifecycle: %v", fx.lifecycle.kills)
	}
}

// TestConnectChildScopedGateRefusedShapes proves the ONLY child credential
// with childScoped reach is the per-child secret: the per-boot + session and
// bare per-boot shapes stay PermissionDenied on every one of the nine verbs,
// with the mechanics untouched — the gate refuses before any handler runs.
func TestConnectChildScopedGateRefusedShapes(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	invoke := []struct {
		name    string
		call    func(set func(h http.Header)) error
		touched func() bool
	}{
		{"Spawn", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", ParentChildId: "c_root"})
			set(req.Header())
			_, err := fx.client.Spawn(ctx, req)
			return err
		}, func() bool { return len(fx.lifecycle.spawns) > 0 }},
		{"ListChildren", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.ListChildrenRequest{})
			set(req.Header())
			_, err := fx.client.ListChildren(ctx, req)
			return err
		}, func() bool { return false }},
		{"GetChild", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_kid"})
			set(req.Header())
			_, err := fx.client.GetChild(ctx, req)
			return err
		}, func() bool { return false }},
		{"GetHistory", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_kid"})
			set(req.Header())
			_, err := fx.client.GetHistory(ctx, req)
			return err
		}, func() bool { return len(fx.history.loads) > 0 }},
		{"StreamEvents", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.StreamEventsRequest{
				Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_kid"}},
			})
			set(req.Header())
			stream, err := fx.client.StreamEvents(ctx, req)
			if err != nil {
				return err
			}
			return receiveToEnd(stream)
		}, func() bool { return false }},
		{"Send", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: "c_kid", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT,
				Blocks: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}}}},
			})
			set(req.Header())
			_, err := fx.client.Send(ctx, req)
			return err
		}, func() bool { return len(fx.inbox.kids) > 0 }},
		{"Kill", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_kid"})
			set(req.Header())
			_, err := fx.client.Kill(ctx, req)
			return err
		}, func() bool { return len(fx.lifecycle.kills) > 0 }},
		{"Close", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_kid"})
			set(req.Header())
			_, err := fx.client.Close(ctx, req)
			return err
		}, func() bool { return len(fx.lifecycle.closes) > 0 }},
		{"ListTasks", func(set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.ListTasksRequest{ConversationId: "conv-c_kid"})
			set(req.Header())
			_, err := fx.client.ListTasks(ctx, req)
			return err
		}, func() bool { return len(fx.tasks.convs) > 0 }},
	}

	for _, cred := range gateRefusedCredentials() {
		for _, verb := range invoke {
			t.Run(cred.name+"/"+verb.name, func(t *testing.T) {
				err := verb.call(cred.set)
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("%s with a %s credential = %v, want %v",
						verb.name, cred.name, err, connect.CodePermissionDenied)
				}
				if verb.touched() {
					t.Fatalf("%s with a %s credential reached its mechanics", verb.name, cred.name)
				}
			})
		}
	}
}

// TestConnectChildScopedListChildren proves the subtree shape of the answer:
// exactly the caller's descendants — never the caller, the sibling, the
// parent or the stranger tree — and the status filter still applies. It also
// pins the control direction: a user credential still sees the whole fleet.
func TestConnectChildScopedListChildren(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	req := connect.NewRequest(&rafikiv1.ListChildrenRequest{})
	childCaller(req.Header())
	resp, err := fx.client.ListChildren(ctx, req)
	if err != nil {
		t.Fatalf("ListChildren as child: %v", err)
	}
	var got []string
	for _, c := range resp.Msg.GetChildren() {
		got = append(got, c.GetChildId())
	}
	// The subtree set, not the store's order: ListChildren promises membership,
	// and the store's list order is an implementation detail.
	sort.Strings(got)
	want := []string{"c_grand", "c_kid"}
	if len(got) != len(want) {
		t.Fatalf("children = %v, want %v (only the subtree)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("children = %v, want %v", got, want)
		}
	}

	// The status filter passes through to the subtree query.
	filtered := connect.NewRequest(&rafikiv1.ListChildrenRequest{Statuses: []string{"running"}})
	childCaller(filtered.Header())
	resp, err = fx.client.ListChildren(ctx, filtered)
	if err != nil {
		t.Fatalf("ListChildren filtered as child: %v", err)
	}
	if len(resp.Msg.GetChildren()) != 0 {
		t.Fatalf("running-only subtree = %d rows, want 0", len(resp.Msg.GetChildren()))
	}

	// Control: the operator still sees the whole fleet.
	user := connect.NewRequest(&rafikiv1.ListChildrenRequest{})
	userCaller(user.Header())
	resp, err = fx.client.ListChildren(ctx, user)
	if err != nil {
		t.Fatalf("ListChildren as user: %v", err)
	}
	if len(resp.Msg.GetChildren()) != 6 {
		t.Fatalf("user ListChildren = %d rows, want 6 (whole fleet)", len(resp.Msg.GetChildren()))
	}
}

// TestConnectChildScopedSpawnForcesParent proves the spawn-side rule: a
// per-child caller's ParentChildID is FORCED to its own id — a client-supplied
// parent (or none) never survives — while a user credential's is passed
// through untouched.
func TestConnectChildScopedSpawnForcesParent(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	// A child credential naming someone else's parent id: forced to c_mine.
	req := connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", ParentChildId: "c_root", Name: "w"})
	childCaller(req.Header())
	if _, err := fx.client.Spawn(ctx, req); err != nil {
		t.Fatalf("Spawn as child: %v", err)
	}
	if len(fx.lifecycle.spawns) != 1 {
		t.Fatalf("spawn reached mechanics %d times, want 1", len(fx.lifecycle.spawns))
	}
	if got := fx.lifecycle.spawns[0].ParentChildID; got != "c_mine" {
		t.Fatalf("ParentChildID = %q, want forced c_mine", got)
	}

	// No parent at all: still forced.
	fx.lifecycle.spawns = nil
	bare := connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Name: "w"})
	childCaller(bare.Header())
	if _, err := fx.client.Spawn(ctx, bare); err != nil {
		t.Fatalf("Spawn without parent as child: %v", err)
	}
	if got := fx.lifecycle.spawns[0].ParentChildID; got != "c_mine" {
		t.Fatalf("ParentChildID = %q, want forced c_mine", got)
	}

	// Control: the operator's parent id rides through untouched.
	fx.lifecycle.spawns = nil
	user := connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", ParentChildId: "c_root", Name: "w"})
	userCaller(user.Header())
	if _, err := fx.client.Spawn(ctx, user); err != nil {
		t.Fatalf("Spawn as user: %v", err)
	}
	if got := fx.lifecycle.spawns[0].ParentChildID; got != "c_root" {
		t.Fatalf("user spawn ParentChildID = %q, want c_root passed through", got)
	}
}

// TestConnectChildScopedListTasks proves the ledger's subtree boundary: a
// child credential may name its own conversation or a descendant's, and
// nothing else — an empty conversation_id (every conversation) is refused
// outright, not answered as an empty list.
func TestConnectChildScopedListTasks(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	cases := []struct {
		name string
		conv string
		want bool
	}{
		{"own ledger", "conv-c_mine", true},
		{"descendant ledger", "conv-c_kid", true},
		{"grand-descendant ledger", "conv-c_grand", true},
		{"empty (every conversation)", "", false},
		{"sibling ledger", "conv-c_sib", false},
		{"parent ledger", "conv-c_root", false},
		{"stranger ledger", "conv-c_stranger", false},
		{"unknown conversation", "conv-nonexistent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx.tasks.convs = nil
			req := connect.NewRequest(&rafikiv1.ListTasksRequest{ConversationId: tc.conv})
			childCaller(req.Header())
			_, err := fx.client.ListTasks(ctx, req)
			if tc.want {
				if err != nil {
					t.Fatalf("ListTasks(%q) = %v, want allowed", tc.conv, err)
				}
				if len(fx.tasks.convs) != 1 || fx.tasks.convs[0] != tc.conv {
					t.Fatalf("ledger saw %v, want [%q]", fx.tasks.convs, tc.conv)
				}
				return
			}
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("ListTasks(%q) = %v, want PermissionDenied", tc.conv, err)
			}
			if len(fx.tasks.convs) != 0 {
				t.Fatalf("refused ListTasks reached the ledger: %v", fx.tasks.convs)
			}
		})
	}

	// Control: the operator still reads every conversation with no filter.
	fx.tasks.convs = nil
	user := connect.NewRequest(&rafikiv1.ListTasksRequest{})
	userCaller(user.Header())
	if _, err := fx.client.ListTasks(ctx, user); err != nil {
		t.Fatalf("ListTasks as user: %v", err)
	}
	if len(fx.tasks.convs) != 1 || fx.tasks.convs[0] != "" {
		t.Fatalf("user ledger saw %v, want the unfiltered pass-through", fx.tasks.convs)
	}
}

// TestConnectChildScopedStreamEventsRefusesAllSubject pins the one
// StreamEvents rule the target matrix cannot express: the daemon-wide All
// subject is operator-only for a child credential, whatever its subtree.
func TestConnectChildScopedStreamEventsRefusesAllSubject(t *testing.T) {
	fx := mountChildScope(t)
	ctx := context.Background()

	req := connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{}},
	})
	childCaller(req.Header())
	stream, err := fx.client.StreamEvents(ctx, req)
	if err != nil {
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("StreamEvents all-subject as child = %v, want %v", err, connect.CodePermissionDenied)
		}
		return
	}
	if err := receiveToEnd(stream); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("StreamEvents all-subject as child = %v, want %v", err, connect.CodePermissionDenied)
	}

	// Control: the operator still streams everything (the stream ends
	// immediately — no event source wired — which is the handler running,
	// not the gate refusing).
	user := connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{}},
	})
	userCaller(user.Header())
	stream, err = fx.client.StreamEvents(ctx, user)
	if err != nil {
		t.Fatalf("StreamEvents all-subject as user: %v", err)
	}
	if err := receiveToEnd(stream); err != nil {
		t.Fatalf("user all-subject stream = %v, want a clean end", err)
	}
}

// TestChildScopeFor pins the source's identity branches, including the one
// fail-open shape it must never produce: a per-child credential whose row has
// left the childstore resolves a scope that refuses everything, NEVER nil
// (nil is the operator path, and a vanished child must not be upgraded to an
// operator).
func TestChildScopeFor(t *testing.T) {
	ctrl := childTree(t)
	ctx := context.Background()

	if sc := ctrl.childScopeFor(ctx); sc != nil {
		t.Fatalf("nil identity resolved %v, want the operator path", sc)
	}
	if sc := ctrl.childScopeFor(server.WithIdentity(ctx, &server.Identity{
		UserID: "u1", Username: "brent", Via: server.ProvenanceUser,
	})); sc != nil {
		t.Fatalf("user credential resolved %v, want the operator path", sc)
	}
	if sc := ctrl.childScopeFor(server.WithIdentity(ctx, &server.Identity{
		UserID: "u1", Via: server.ProvenanceChildAttributed,
	})); sc != nil {
		t.Fatalf("per-boot+session resolved %v, want the operator path", sc)
	}

	sc := ctrl.childScopeFor(server.WithIdentity(ctx, &server.Identity{
		UserID: "u1", ChildID: "c_mine", Via: server.ProvenanceChildToken,
	}))
	if sc == nil {
		t.Fatal("per-child secret resolved nil, want a subtree scope")
	}
	if sc.ChildID() != "c_mine" {
		t.Fatalf("ChildID = %q, want c_mine", sc.ChildID())
	}
	if err := sc.Authorize("c_kid"); err != nil {
		t.Fatalf("Authorize(descendant) = %v, want nil", err)
	}

	// The vanished row: non-nil scope, everything refused.
	sc = ctrl.childScopeFor(server.WithIdentity(ctx, &server.Identity{
		UserID: "u1", ChildID: "c_gone", Via: server.ProvenanceChildToken,
	}))
	if sc == nil {
		t.Fatal("vanished child resolved nil, want an always-refusing scope")
	}
	if err := sc.Authorize("c_kid"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("vanished child Authorize = %v, want %v", err, connect.CodePermissionDenied)
	}
	if got := sc.Subtree(nil); len(got) != 0 {
		t.Fatalf("vanished child Subtree = %v, want empty", got)
	}
	if sc.ConversationInScope("conv-c_mine") {
		t.Fatal("vanished child admitted a conversation")
	}
}
