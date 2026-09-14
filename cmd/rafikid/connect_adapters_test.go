// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
)

// capturingCoster records the selector it was handed.
type capturingCoster struct {
	sel  insights.SubtreeSelector
	rows []insights.ConversationCost
}

func (c *capturingCoster) SubtreeCost(context.Context, insights.SubtreeSelector) (float64, error) {
	return 0, nil
}

func (c *capturingCoster) CostsByConversation(
	_ context.Context, sel insights.SubtreeSelector,
) ([]insights.ConversationCost, error) {
	c.sel = sel
	return c.rows, nil
}

// The two correlation routes are not interchangeable. A conversation is found
// by UUID for a fundi child (SessionID) and by external_ref for a proxy child,
// where the daemon sets X-Rafiki-Session to the CHILD id. Handing SessionID to
// both routes makes every proxy child match neither and roll up a non-nil
// zero, which then overwrites cost the rail accumulated from turn_end.
func TestCostsForCorrelatesExternalRefByChildID(t *testing.T) {
	cap := &capturingCoster{}
	c := &Controller{coster: cap}

	c.costsFor([]childstore.Snapshot{
		{ChildID: "c_fundi", SessionID: "11111111-1111-1111-1111-111111111111"},
		{ChildID: "c_proxy"},
	})

	if got, want := cap.sel.ConversationIDs, []string{"11111111-1111-1111-1111-111111111111"}; !slices.Equal(got, want) {
		t.Errorf("ConversationIDs = %v, want %v (the SESSION id)", got, want)
	}
	if got, want := cap.sel.ExternalRefs, []string{"c_fundi", "c_proxy"}; !slices.Equal(got, want) {
		t.Errorf("ExternalRefs = %v, want %v (the CHILD id, for every child)", got, want)
	}
}

// One round trip for the whole list, not one per child.
func TestCostsForIssuesASingleRollup(t *testing.T) {
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "22222222-2222-2222-2222-222222222222", Cost: 2.0},
		{ConversationID: "33333333-3333-3333-3333-333333333333", ExternalRef: "c_proxy", Cost: 5.0},
	}}
	c := &Controller{coster: cap}

	got := c.costsFor([]childstore.Snapshot{
		{ChildID: "c_fundi", SessionID: "22222222-2222-2222-2222-222222222222"},
		{ChildID: "c_proxy"},
		{ChildID: "c_idle", SessionID: "44444444-4444-4444-4444-444444444444"},
	})

	if got["c_fundi"] != 2.0 {
		t.Errorf("c_fundi = %v, want 2.0 (matched by conversation UUID)", got["c_fundi"])
	}
	if got["c_proxy"] != 5.0 {
		t.Errorf("c_proxy = %v, want 5.0 (matched by external_ref)", got["c_proxy"])
	}
	// Present and zero is a real answer -- the query ran and found no turns --
	// and is distinct from absent, which leaves CostUSD nil.
	if v, ok := got["c_idle"]; !ok || v != 0 {
		t.Errorf("c_idle = (%v, %v), want (0, true)", v, ok)
	}
}

// A child reachable by BOTH routes resolves to one conversation row and must
// be counted once.
func TestCostsForCountsOneConversationOnce(t *testing.T) {
	conv := "55555555-5555-5555-5555-555555555555"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: conv, ExternalRef: "c_both", Cost: 4.0},
	}}
	c := &Controller{coster: cap}

	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_both", SessionID: conv}})
	if got["c_both"] != 4.0 {
		t.Errorf("c_both = %v, want 4.0: matching both routes must not double it", got["c_both"])
	}
}

// A thread branch with no child of its own is a tool call the parent made --
// Claude Code's WebFetch summarizer and WebSearch driver fork a branch each and
// get no synthetic child, because they declare no client tools and so are not
// agents. Their spend must land on the parent: TOTAL is summed from child rows,
// so a branch nothing claims is money that silently leaves the report.
func TestCostsForRollsUnclaimedBranchesIntoTheParent(t *testing.T) {
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "66666666-6666-6666-6666-666666666666",
			ExternalRef: "c_parent", Cost: 1.0},
		{ConversationID: "77777777-7777-7777-7777-777777777777",
			ExternalRef: "c_parent:aaaa", Cost: 0.25},
		{ConversationID: "88888888-8888-8888-8888-888888888888",
			ExternalRef: "c_parent:bbbb", Cost: 0.75},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	c := &Controller{coster: cap, st: st}

	// The prefix route is what reaches those branches at all; without it the
	// query never returns them and there is nothing to attribute.
	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_parent"}})
	if !slices.Contains(cap.sel.ExternalRefPrefixes, "c_parent:") {
		t.Errorf("ExternalRefPrefixes = %v, want it to carry %q",
			cap.sel.ExternalRefPrefixes, "c_parent:")
	}
	if got["c_parent"] != 2.0 {
		t.Errorf("c_parent = %v, want 2.0 (own 1.0 plus two unclaimed branches)",
			got["c_parent"])
	}
}

// A branch a real subagent DOES claim is that subagent's spend, not the
// parent's -- otherwise every Task subagent's cost is reported twice, once on
// its own row and once folded into its parent's.
func TestCostsForLeavesAClaimedBranchOnItsOwnChild(t *testing.T) {
	branch := "c_parent:cccc"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "99999999-9999-9999-9999-999999999999",
			ExternalRef: "c_parent", Cost: 1.0},
		{ConversationID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			ExternalRef: branch, Cost: 3.0},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	st.Insert(&childstore.Session{ChildID: branch, Native: true})
	c := &Controller{coster: cap, st: st}

	got := c.costsFor([]childstore.Snapshot{
		{ChildID: "c_parent"},
		{ChildID: branch},
	})
	if got["c_parent"] != 1.0 {
		t.Errorf("c_parent = %v, want 1.0: a claimed branch is its own child's spend",
			got["c_parent"])
	}
	if got[branch] != 3.0 {
		t.Errorf("%s = %v, want 3.0", branch, got[branch])
	}
}

// Claimed-ness is a property of the CHILDSTORE, never of the snapshot slice:
// snaps is routinely status-filtered, and a real subagent filtered out of the
// list must not have its cost slide onto its parent as if it were a helper.
func TestCostsForChecksClaimsAgainstTheStoreNotTheFilteredList(t *testing.T) {
	branch := "c_parent:dddd"
	cap := &capturingCoster{rows: []insights.ConversationCost{
		{ConversationID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			ExternalRef: branch, Cost: 3.0},
	}}
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent"})
	st.Insert(&childstore.Session{ChildID: branch, Native: true})
	c := &Controller{coster: cap, st: st}

	// Only the parent is listed -- the subagent exists but was filtered out.
	got := c.costsFor([]childstore.Snapshot{{ChildID: "c_parent"}})
	if got["c_parent"] != 0 {
		t.Errorf("c_parent = %v, want 0: a filtered-out subagent still owns its branch",
			got["c_parent"])
	}
}

// No cost source means NOT KNOWN, which must leave CostUSD nil rather than
// reporting a zero the rail would then adopt.
func TestCostsForWithNoCosterIsAbsentNotZero(t *testing.T) {
	c := &Controller{}
	if got := c.costsFor([]childstore.Snapshot{{ChildID: "c1"}}); got != nil {
		t.Errorf("costsFor with no coster = %v, want nil", got)
	}
}

// ─── Conversation query adapters (Connect plane) ─────────────────────────────

// fakeInsightsBackend stands in for the agentcli.Backend the Controller's
// insights field holds. Only Search and Export are reachable: the embedded
// interface is nil, so an unexpected call panics rather than passing
// silently.
type fakeInsightsBackend struct {
	agentcli.Backend

	gotScope   insights.Scope
	gotExport  string
	transcript *insights.Transcript
	exportErr  error

	searchRows []insights.ConversationSummary
	searchErr  error
}

func (f *fakeInsightsBackend) Search(_ context.Context, scope insights.Scope, _ insights.SearchFilter) ([]insights.ConversationSummary, error) {
	f.gotScope = scope
	return f.searchRows, f.searchErr
}

func (f *fakeInsightsBackend) Export(_ context.Context, scope insights.Scope, id string) (*insights.Transcript, error) {
	f.gotScope = scope
	f.gotExport = id
	return f.transcript, f.exportErr
}

// TestConversationReadNotFoundClassifiesTheControllersNotFoundAnswer covers
// conversationReadNotFound directly. The ControllerError branch is the one
// that matters: translateInsightsErr folds the sentinel into a
// ControllerError that does not Unwrap, so the code comparison is what makes
// "a scope miss reads as not-found" hold at all.
func TestConversationReadNotFoundClassifiesTheControllersNotFoundAnswer(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"controller not-found", &control.ControllerError{
			Code: protocol.ErrNotFound, Message: "export: conversation x: insights: conversation not found",
		}, true},
		{"wrapped controller not-found", fmt.Errorf("load: %w",
			&control.ControllerError{Code: protocol.ErrNotFound}), true},
		{"raw sentinel", insights.ErrNotFound, true},
		{"controller internal", &control.ControllerError{
			Code: protocol.ErrInternal, Message: "boom",
		}, false},
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := conversationReadNotFound(tc.err); got != tc.want {
			t.Errorf("%s: conversationReadNotFound(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestConversationExportAdapterFoldsNotFoundIntoOkFalse pins the full chain
// as ONE property: Controller.ConversationExport -> translateInsightsErr ->
// conversationReadNotFound -> ok=false, err=nil. A scope miss and a missing
// conversation must be indistinguishable all the way out of the adapter, and
// the caller's scope must actually have been threaded down to the backend.
func TestConversationExportAdapterFoldsNotFoundIntoOkFalse(t *testing.T) {
	fb := &fakeInsightsBackend{exportErr: insights.ErrNotFound}
	a := connectConversations{c: &Controller{insights: fb}}

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
	tr, ok, err := a.Export(ctx, "conv-1")
	if ok || err != nil || tr.ConversationID != "" || len(tr.Turns) != 0 {
		t.Fatalf("Export not-found = (ok=%v, err=%v, tr=%+v), want (false, nil, zero)", ok, err, tr)
	}
	if fb.gotScope != insights.ScopeOwner("u1") {
		t.Errorf("backend scope = %v, want ScopeOwner(u1)", fb.gotScope)
	}
	if fb.gotExport != "conv-1" {
		t.Errorf("backend got conversation id %q, want conv-1", fb.gotExport)
	}
}

// TestConversationSearchAdapterMapsNoAgentDB pins the ControllerError
// translation end to end: the backend's ErrNoPool is promoted by
// translateInsightsErr to a ControllerError carrying a curated, actionable
// message, which must reach the caller as FailedPrecondition with that text
// -- not redacted (it is ours) and not Internal (the request is fine; the
// daemon is unconfigured).
func TestConversationSearchAdapterMapsNoAgentDB(t *testing.T) {
	fb := &fakeInsightsBackend{searchErr: local.ErrNoPool}
	a := connectConversations{c: &Controller{insights: fb}}

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser})
	_, err := a.Search(ctx, connectapi.ConversationSearchFilter{})
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("Search ErrNoPool err = %v, want CodeFailedPrecondition", err)
	}
	if !strings.Contains(ce.Message(), "no agent database configured") {
		t.Errorf("message = %q, want the curated no-agent-db text", ce.Message())
	}
}

// TestControllerConnectCodeCoversTheProtocolCodes pins the code table so a
// new ControllerError code fails here instead of silently degrading to
// Internal (with its curated message intact, but the wrong code).
func TestControllerConnectCodeCoversTheProtocolCodes(t *testing.T) {
	cases := map[string]connect.Code{
		protocol.ErrNotFound:  connect.CodeNotFound,
		protocol.ErrNoAgentDB: connect.CodeFailedPrecondition,
		protocol.ErrInternal:  connect.CodeInternal,
		"some future code":    connect.CodeInternal,
	}
	for code, want := range cases {
		if got := controllerConnectCode(code); got != want {
			t.Errorf("controllerConnectCode(%q) = %v, want %v", code, got, want)
		}
	}
}

// TestScopeForRefusesANonUserCredential covers both refusals: no identity at
// all, and a child-attributed identity. The second is the case the design's
// §3 warns about -- it CARRIES the owner's UserID (here "u1"), so a
// non-empty-UserID check would hand the agent its owner's whole corpus.
func TestScopeForRefusesANonUserCredential(t *testing.T) {
	if _, err := scopeFor(context.Background()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("scopeFor(nil identity) err = %v, want %v", err, connect.CodePermissionDenied)
	}
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed})
	if _, err := scopeFor(ctx); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("scopeFor(child-attributed with owner UserID) err = %v, want %v", err, connect.CodePermissionDenied)
	}
}

// TestScopeForMapsAUserCredentialOntoItsOwnScope pins the positive path.
func TestScopeForMapsAUserCredentialOntoItsOwnScope(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
	got, err := scopeFor(ctx)
	if err != nil {
		t.Fatalf("scopeFor(user) err = %v", err)
	}
	if got != insights.ScopeOwner("u1") {
		t.Errorf("scopeFor(user u1) = %v, want ScopeOwner(u1)", got)
	}
}

// TestScopeForMapsAnAdminOntoAll pins the admin path.
func TestScopeForMapsAnAdminOntoAll(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser, IsAdmin: true})
	got, err := scopeFor(ctx)
	if err != nil {
		t.Fatalf("scopeFor(admin) err = %v", err)
	}
	if got != insights.ScopeAll() {
		t.Errorf("scopeFor(admin) = %v, want ScopeAll()", got)
	}
}

// TestConversationSearchHandlerPreservesTheRefusedCredentialCode drives a
// child-attributed identity through the real ConversationSearch handler with
// the production adapter wired: the returned error must still be
// permission_denied. This is the pass-through property the handler's
// queryError exists to keep -- before it, the blanket CodeInternal wrap
// turned every refusal into an internal error.
func TestConversationSearchHandlerPreservesTheRefusedCredentialCode(t *testing.T) {
	srv := connectapi.NewServer(nil)
	srv.SetConversationInsights(connectConversations{c: &Controller{insights: &fakeInsightsBackend{}}})

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed})
	_, err := srv.ConversationSearch(ctx,
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("handler refused credential err = %v, want permission_denied (not re-wrapped internal)", err)
	}
}

// TestConversationSearchOverUDSRefusesAChildAttributedCredential is the
// wire-level proof for the same property: the per-boot child secret plus
// X-Rafiki-Session resolves to a child-attributed identity, and the RPC that
// identity reaches must answer permission_denied on the wire, never
// internal.
func TestConversationSearchOverUDSRefusesAChildAttributedCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	auth := server.NewUserTokenAuth(stubUserStore{}, "child-boot-secret", time.Minute)
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_child" {
			return "u1", true
		}
		return "", false
	})

	srv := connectapi.NewServer(nil)
	srv.SetConversationInsights(connectConversations{c: &Controller{insights: &fakeInsightsBackend{}}})

	// t.TempDir() embeds the test NAME, and this one is long enough to push
	// the socket path past the sun_path limit (macOS 104) -- hence a short
	// custom dir rather than the pattern the sibling UDS tests use.
	dir, err := os.MkdirTemp("", "cuds")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	req := connect.NewRequest(&rafikiv1.ConversationSearchRequest{})
	req.Header().Set("Authorization", "Bearer child-boot-secret")
	req.Header().Set("X-Rafiki-Session", "c_child")
	if _, err := client.ConversationSearch(ctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("ConversationSearch over UDS with a child-attributed credential err = %v, want permission_denied", err)
	}
}
