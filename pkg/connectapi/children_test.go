// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

type fakeLister struct {
	all        []protocol.ChildSummary
	gotStatus  []string
	lookupMiss bool
}

func (f *fakeLister) ListChildren(statuses []string) []protocol.ChildSummary {
	f.gotStatus = statuses
	return f.all
}

func (f *fakeLister) GetChild(childID string) (protocol.ChildSummary, bool) {
	if f.lookupMiss {
		return protocol.ChildSummary{}, false
	}
	for _, c := range f.all {
		if c.ChildID == childID {
			return c, true
		}
	}
	return protocol.ChildSummary{}, false
}

func sampleChildren() []protocol.ChildSummary {
	pid := 4242
	return []protocol.ChildSummary{{
		ChildID: "c_1", Name: "scout", Kind: "fundi", Status: "idle",
		Model: "claude-opus-5", Cwd: "/tmp", PID: &pid,
		StartedAt: time.UnixMilli(100), LastActivity: time.UnixMilli(200), SessionID: "conv-uuid",
		Labels: map[string]string{"rafiki/parent": "c_0"}, ContextWindow: 200000,
	}}
}

func TestListChildrenMapsFields(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: sampleChildren()})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	ck.Require().NoError(err, "ListChildren")
	ck.Require().Len(resp.Msg.GetChildren(), 1, "children = %d, want 1", len(resp.Msg.GetChildren()))
	c := resp.Msg.GetChildren()[0]
	ck.False(c.GetChildId() != "c_1" || c.GetName() != "scout" || c.GetKind() != "fundi", "identity fields wrong: %+v", c)
	ck.False(c.GetStatus() != "idle" || c.GetModel() != "claude-opus-5" || c.GetCwd() != "/tmp", "state fields wrong: %+v", c)
	ck.Eq(4242, c.GetPid(), "Pid")
	ck.False(c.GetStartedAt() == nil || !c.GetStartedAt().AsTime().Equal(time.UnixMilli(100)) || c.GetLastActivity() == nil || !c.GetLastActivity().AsTime().Equal(time.UnixMilli(200)), "timestamps wrong: %+v", c)
	ck.False(c.GetSessionId() != "conv-uuid" || c.GetContextWindow() != 200000, "session/window wrong: %+v", c)
	ck.Eq("c_0", c.GetLabels()["rafiki/parent"], "labels wrong: %+v", c.GetLabels())
}

// TestListChildrenNilPidStaysNil proves the optional field survives: an exited
// child has no pid, and 0 is a legal pid value.
func TestListChildrenCarriesLatestOrdinal(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	elog := eventlog.NewMemory()
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "idle"))
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "running"))

	s := connectapi.NewServer(nil)
	s.SetEventLog(elog)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{
		{ChildID: "c_1"},
		{ChildID: "c_empty"},
	}})

	resp, err := s.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	ck.NoError(err, "ListChildren")
	children := resp.Msg.GetChildren()
	ck.Len(children, 2, "got %d children, want 2", len(children))
	var c1, cEmpty *rafikiv1.ChildSummary
	for _, c := range children {
		if c.GetChildId() == "c_1" {
			c1 = c
		} else if c.GetChildId() == "c_empty" {
			cEmpty = c
		}
	}
	if c1 == nil || c1.LatestOrdinal == nil || *c1.LatestOrdinal != 1 {
		t.Fatalf("c_1 LatestOrdinal = %v, want 1", c1.LatestOrdinal)
	}
	if cEmpty == nil || cEmpty.LatestOrdinal != nil {
		t.Fatalf("c_empty LatestOrdinal = %v, want nil", cEmpty.LatestOrdinal)
	}
}
func TestListChildrenNilPidStaysNil(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{ChildID: "c_1"}}})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	c.Require().NoError(err, "ListChildren")
	c.Nil(resp.Msg.GetChildren()[0].Pid, "Pid must stay nil when the source PID is nil")
}

func TestListChildrenForwardsStatusFilter(t *testing.T) {
	f := &fakeLister{all: sampleChildren()}
	s := connectapi.NewServer(nil)
	s.SetChildLister(f)

	_, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Statuses: []string{"idle", "streaming"}}))
	assert.NewAborting(t).NoError(err, "ListChildren")
	if len(f.gotStatus) != 2 || f.gotStatus[0] != "idle" || f.gotStatus[1] != "streaming" {
		t.Errorf("forwarded statuses = %v, want [idle streaming]", f.gotStatus)
	}
}

func TestListChildrenWithoutListerFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestGetChildReturnsOne(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: sampleChildren()})

	resp, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_1"}))
	c.Require().NoError(err, "GetChild")
	c.Eq("c_1", resp.Msg.GetChild().GetChildId(), "ChildId")
}

func TestGetChildUnknownIsNotFound(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{lookupMiss: true})

	_, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"}))
	assert.NewCollecting(t).Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
}

// TestListChildrenCarriesTheNewSummaryFields pins ChildSummary parity: the
// four fields wave 1 added to the wire type (session_file, exit_signal,
// slash_commands, max_completion_tokens) must reach the client from the same
// protocol.ChildSummary source pkg/control's SnapshotToSummary populates.
func TestListChildrenCarriesTheNewSummaryFields(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{
		ChildID: "c_1", SessionFile: "/state/c_1.jsonl", ExitSignal: "KILL",
		SlashCommands: []string{"/compact", "/clear"}, MaxCompletionTokens: 8192,
	}}})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	ck.Require().NoError(err, "ListChildren")
	c := resp.Msg.GetChildren()[0]
	ck.Eq("/state/c_1.jsonl", c.GetSessionFile(), "SessionFile")
	ck.Eq("KILL", c.GetExitSignal(), "ExitSignal")
	if len(c.GetSlashCommands()) != 2 || c.GetSlashCommands()[0] != "/compact" {
		t.Errorf("SlashCommands = %v, want [/compact /clear]", c.GetSlashCommands())
	}
	ck.Eq(8192, c.GetMaxCompletionTokens(), "MaxCompletionTokens")
}

func TestGetChildRejectsEmptyID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{})
	_, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// TestChildSummaryCarriesDisplayState pins the operator display tuple onto the
// wire: the daemon's protocol.ChildSummary display fields must all reach the
// client's proto ChildSummary.
func TestChildSummaryCarriesDisplayState(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{
		ChildID: "c_1", Status: "idle",
		DisplayState: protocol.DisplayWaiting,
		WaitingOn:    protocol.WaitingOnOperator,
		DisplayMsg:   "proceed?",
	}}})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	ck.Require().NoError(err, "ListChildren")
	c := resp.Msg.GetChildren()[0]
	ck.Eq("waiting", c.GetDisplayState(), "DisplayState")
	ck.Eq("operator", c.GetWaitingOn(), "WaitingOn")
	ck.Eq("proceed?", c.GetDisplayMsg(), "DisplayMsg")
}

// readScopeChildScope is a ChildScope whose two authorities are SEPARATE sets,
// so a test can prove that reading widened to the caller's owner while acting
// did not: Authorize is the act check (the spawn tree) and AuthorizeRead the
// read check (the subtree OR the owner's agents). The two list halves are fixed
// values too, because ListChildren's job here is choosing which one to ask for;
// the filtering and ordering behind them is the daemon's childScope, pinned in
// cmd/rafikid.
type readScopeChildScope struct {
	id       string
	subtree  []protocol.ChildSummary
	owned    []protocol.ChildSummary
	actable  map[string]bool
	readable map[string]bool
}

func (s readScopeChildScope) ChildID() string { return s.id }

func (s readScopeChildScope) Authorize(target string) error {
	if s.actable[target] {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errTestNotDescendant(target))
}

func (s readScopeChildScope) AuthorizeRead(target string) error {
	if s.readable[target] {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("agent %s is not one you can read", target))
}

func (s readScopeChildScope) Subtree([]string) []protocol.ChildSummary { return s.subtree }

func (s readScopeChildScope) OwnedChildren([]string) []protocol.ChildSummary { return s.owned }

func (s readScopeChildScope) ConversationInScope(string) bool { return false }

// childIDsInOrder is the answer's ids in the order the handler emitted them —
// the owner-scope list's whole promise is subtree-first, so order is asserted,
// not just membership.
func childIDsInOrder(cs []*rafikiv1.ChildSummary) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.GetChildId())
	}
	return out
}

// listScopes is the three wire values a caller may name.
var listScopes = []rafikiv1.ListScope{
	rafikiv1.ListScope_LIST_SCOPE_UNSPECIFIED,
	rafikiv1.ListScope_LIST_SCOPE_SUBTREE,
	rafikiv1.ListScope_LIST_SCOPE_OWNER,
}

// TestListChildrenScopeUnspecifiedAndSubtreeReturnSubtree pins the default and
// the explicit narrow value: both answer the caller's subtree, and neither
// claims an in_subtree flag (the flag describes the OWNER list, where a row can
// be readable without being the caller's).
func TestListChildrenScopeUnspecifiedAndSubtreeReturnSubtree(t *testing.T) {
	c := assert.NewCollecting(t)
	yes := true
	sc := readScopeChildScope{
		id:      "c_caller",
		subtree: []protocol.ChildSummary{{ChildID: "c_kid"}},
		owned: []protocol.ChildSummary{
			{ChildID: "c_kid", InSubtree: &yes},
			{ChildID: "c_sib"},
		},
	}
	s := connectapi.NewServer(nil)
	// The childScoped branch still needs a lister wired: the Unavailable guard
	// runs before the scope branch.
	s.SetChildLister(&fakeLister{})
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return sc })

	for _, scope := range []rafikiv1.ListScope{
		rafikiv1.ListScope_LIST_SCOPE_UNSPECIFIED,
		rafikiv1.ListScope_LIST_SCOPE_SUBTREE,
	} {
		resp, err := s.ListChildren(context.Background(),
			connect.NewRequest(&rafikiv1.ListChildrenRequest{Scope: scope}))
		c.Require().NoError(err, "ListChildren scope=%v", scope)
		c.EqDeep([]string{"c_kid"}, childIDsInOrder(resp.Msg.GetChildren()),
			"scope %v must answer the subtree", scope)
		for _, row := range resp.Msg.GetChildren() {
			c.Nil(row.InSubtree, "scope %v set in_subtree on %s", scope, row.GetChildId())
		}
	}
}

// TestListChildrenScopeOwnerReturnsOwnedChildren pins the widening: OWNER asks
// the scope for the owner list instead of the subtree, keeps its order
// (subtree rows first), and carries in_subtree onto the wire — which is what
// fails if toProtoChild stops copying it.
func TestListChildrenScopeOwnerReturnsOwnedChildren(t *testing.T) {
	c := assert.NewCollecting(t)
	yes, no := true, false
	sc := readScopeChildScope{
		id:      "c_caller",
		subtree: []protocol.ChildSummary{{ChildID: "c_kid"}},
		owned: []protocol.ChildSummary{
			{ChildID: "c_kid", InSubtree: &yes},
			{ChildID: "c_sib", InSubtree: &no},
		},
	}
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{})
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return sc })

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Scope: rafikiv1.ListScope_LIST_SCOPE_OWNER}))
	c.Require().NoError(err, "ListChildren scope=OWNER")
	rows := resp.Msg.GetChildren()
	c.EqDeep([]string{"c_kid", "c_sib"}, childIDsInOrder(rows), "the owner list, subtree rows first")

	c.Require().Len(rows, 2, "rows")
	c.NotNil(rows[0].InSubtree, "the caller's own agent carries in_subtree")
	c.True(rows[0].GetInSubtree(), "a spawned agent is in_subtree")
	c.NotNil(rows[1].InSubtree, "a readable sibling carries in_subtree too")
	c.False(rows[1].GetInSubtree(), "a merely-readable agent is not in_subtree")
}

// TestListChildrenScopeUnknownValueIsInvalidArgument pins the guard on the enum
// itself: a value outside the three names no scope at all, so it is refused
// before the lister is consulted rather than answered with the default.
func TestListChildrenScopeUnknownValueIsInvalidArgument(t *testing.T) {
	c := assert.NewCollecting(t)
	lister := &verbsRecordingLister{}
	sc := readScopeChildScope{id: "c_caller"}
	s := connectapi.NewServer(nil)
	s.SetChildLister(lister)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return sc })

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Scope: rafikiv1.ListScope(99)}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "scope=99 = %v, want", err)
	c.Nil(resp, "a refused scope produced a response")
	c.Eq(0, lister.listCalls, "a refused scope consulted the lister")

	// The same refusal for a caller with no position in the tree: the guard is
	// on the request, not on the caller.
	op := connectapi.NewServer(nil)
	op.SetChildLister(lister)
	_, err = op.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Scope: rafikiv1.ListScope(99)}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "operator scope=99 = %v, want", err)
	c.Eq(0, lister.listCalls, "a refused operator scope consulted the lister")
}

// TestListChildrenScopeIsNoOpForNonChildCallers pins the no-position rule: an
// admin, a user credential and the unix socket have no subtree to narrow, so
// all three values answer the identical set and in_subtree stays unset on every
// row (false would be a claim, and they are not making one).
func TestListChildrenScopeIsNoOpForNonChildCallers(t *testing.T) {
	all := []protocol.ChildSummary{{ChildID: "c_1"}, {ChildID: "c_2"}}
	cases := []struct {
		name  string
		shape func(s *connectapi.Server)
	}{
		{"unix socket", func(*connectapi.Server) {}},
		{"user guard", func(s *connectapi.Server) {
			s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard {
				return &fakeOwnerGuard{
					owned: map[string]bool{"c_1": true, "c_2": true},
					convs: map[string]bool{},
				}
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			var want []string
			for _, scope := range listScopes {
				s := connectapi.NewServer(nil)
				s.SetChildLister(&fakeLister{all: all})
				tc.shape(s)
				resp, err := s.ListChildren(context.Background(),
					connect.NewRequest(&rafikiv1.ListChildrenRequest{Scope: scope}))
				c.Require().NoError(err, "%s scope=%v", tc.name, scope)
				ids := childIDsInOrder(resp.Msg.GetChildren())
				if want == nil {
					want = ids
				}
				c.EqDeep(want, ids, "%s scope=%v answered a different set", tc.name, scope)
				for _, row := range resp.Msg.GetChildren() {
					c.Nil(row.InSubtree, "%s scope=%v set in_subtree on %s", tc.name, scope, row.GetChildId())
				}
			}
			c.EqDeep([]string{"c_1", "c_2"}, want, "%s must see its whole visible set", tc.name)
		})
	}
}

// TestGetHistoryReadScopeAdmitsSibling pins GetHistory's boundary as the READ
// one: a target the act check refuses but the read check admits reaches the
// loader. With Authorize in place of AuthorizeRead this fails, which is exactly
// the regression it exists to catch.
func TestGetHistoryReadScopeAdmitsSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	loader := &recordingLoader{}
	s := connectapi.NewServer(loader)
	s.SetChildResolver(fakeResolver{})
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
		return readScopeChildScope{id: "c_caller", readable: map[string]bool{"c_sib": true}}
	})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_sib"}))
	c.NoError(err, "GetHistory of a same-owner sibling = %v, want allowed", err)
	c.Eq("c_sib", loader.gotID, "the admitted read reached the history loader")
}

// TestGetHistoryReadScopeStillRefusesForeign pins the fail-closed half: a
// target the read check refuses is PermissionDenied and never reaches the
// store, so a foreign agent's transcript is not even probed.
func TestGetHistoryReadScopeStillRefusesForeign(t *testing.T) {
	c := assert.NewCollecting(t)
	loader := &recordingLoader{}
	s := connectapi.NewServer(loader)
	s.SetChildResolver(fakeResolver{})
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
		return readScopeChildScope{id: "c_caller", readable: map[string]bool{"c_sib": true}}
	})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_foreign"}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "GetHistory of a foreign agent = %v, want", err)
	c.Eq("", loader.gotID, "a refused read reached the history loader")
}

// TestGetChildReadScopeAdmitsSibling is GetHistory's sibling case on the
// single-child read, and pins that the row comes back at all: the act check
// refuses the sibling, the read check admits it.
func TestGetChildReadScopeAdmitsSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{ChildID: "c_sib", Name: "sib"}}})
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
		return readScopeChildScope{id: "c_caller", readable: map[string]bool{"c_sib": true}}
	})

	resp, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_sib"}))
	c.Require().NoError(err, "GetChild of a same-owner sibling")
	c.Eq("c_sib", resp.Msg.GetChild().GetChildId(), "the sibling row")
}

// TestSendKillSetBudgetStillRefuseSibling is the anti-widening pin: the three
// ACT verbs keep Authorize, so a same-owner sibling that the read check admits
// is still refused — PermissionDenied, with the backend untouched. If any of
// them ever switched to AuthorizeRead this fails.
func TestSendKillSetBudgetStillRefuseSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	inbox := &fakeAccepter{}
	lc := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetInbox(inbox)
	s.SetChildLifecycle(lc)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
		// Readable, never actable: the exact asymmetry this test defends.
		return readScopeChildScope{id: "c_caller", readable: map[string]bool{"c_sib": true}}
	})
	ctx := context.Background()

	_, err := s.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_sib", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("hi"),
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Send to a readable sibling = %v, want", err)
	c.Eq("", inbox.got.ChildID, "a refused Send reached the inbox")

	_, err = s.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_sib"}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Kill of a readable sibling = %v, want", err)
	c.Eq("", lc.killedID, "a refused Kill reached the lifecycle")

	_, err = s.SetBudget(ctx, connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_sib", MaxCost: 1}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "SetBudget on a readable sibling = %v, want", err)
	c.Eq("", lc.budgetChildID, "a refused SetBudget reached the lifecycle")
}

// TestGetChildReadScopeActVerbsStillRefuseSibling is the pattern-prefixed entry
// point the task's verify command runs; the pinned body is the sibling name
// above.
func TestGetChildReadScopeActVerbsStillRefuseSibling(t *testing.T) {
	t.Run("act verbs still refuse a readable sibling", TestSendKillSetBudgetStillRefuseSibling)
}
