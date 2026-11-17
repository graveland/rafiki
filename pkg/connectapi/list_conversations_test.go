// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestListConversationsNotWiredFailsUnavailable pins the unwired answer, the
// same one every other conversation verb gives.
func TestListConversationsNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ListConversations unwired err = %v, want", err)
}

// TestListConversationsMapsEveryFilterField pins that every request field
// reaches the seam, and that the rows and the cursor come back out.
func TestListConversationsMapsEveryFilterField(t *testing.T) {
	c := assert.NewCollecting(t)
	since := time.Unix(1700000000, 0)
	until := time.Unix(1700003600, 0)
	closed := time.Unix(1757000000, 0)
	f := &fakeConversationInsights{
		listNext: "next-1",
		listRows: []connectapi.ConversationListingRow{{
			ID: "conv-1", Name: "fix the lease", Model: "openrouter/x/glm", Owner: "brent",
			CreatedAt: time.Unix(1756900000, 0), LastActivity: time.Unix(1757000000, 0),
			ClosedAt: &closed, Turns: 7, CostUSD: 0.0123,
			ParentConversationID: "conv-0", ChildCount: 3,
			ChildID: "c_1", ChildName: "worker", Kind: "fundi",
			State: connectapi.ConversationListStateExited,
		}, {
			ID: "conv-2", Name: "no agent",
			CreatedAt: time.Unix(1756900000, 0), LastActivity: time.Unix(1757000100, 0),
			State: connectapi.ConversationListStateNoAgent,
		}},
	}
	s := newConversationsServer(f)

	resp, err := s.ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{
			Limit:                20,
			Cursor:               "cur-9",
			Sort:                 rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED,
			ParentConversationId: "conv-0",
			Query:                "lease fencing",
			States:               []rafikiv1.ConversationState{rafikiv1.ConversationState_CONVERSATION_STATE_EXITED},
			Kinds:                []string{"fundi", "claude"},
			NameContains:         "lease",
			CwdContains:          "/home/brent",
			Labels:               map[string]string{"rafiki/parent": "c_0"},
			Since:                timestamppb.New(since),
			Until:                timestamppb.New(until),
		}))
	c.Require().NoError(err, "ListConversations")

	got := f.gotList
	c.True(f.listCalled, "the seam must be called")
	c.Eq(20, got.Limit, "Limit")
	c.Eq("cur-9", got.Cursor, "Cursor")
	c.Eq(connectapi.ConversationListSortCreated, got.Sort, "Sort")
	c.Eq("conv-0", got.ParentConversationID, "ParentConversationID")
	c.Eq("lease fencing", got.Query, "Query")
	c.EqDeep([]connectapi.ConversationListState{connectapi.ConversationListStateExited}, got.States, "States")
	c.EqDeep([]string{"fundi", "claude"}, got.Kinds, "Kinds")
	c.Eq("lease", got.NameContains, "NameContains")
	c.Eq("/home/brent", got.CwdContains, "CwdContains")
	c.EqDeep(map[string]string{"rafiki/parent": "c_0"}, got.Labels, "Labels")
	c.Require().True(got.Since != nil && got.Until != nil, "since/until must reach the seam")
	c.Eq(since.Unix(), got.Since.Unix(), "Since")
	c.Eq(until.Unix(), got.Until.Unix(), "Until")

	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 2, "rows = %d, want 2", len(rows))
	first := rows[0]
	c.Eq("conv-1", first.GetId(), "Id")
	c.Eq("fix the lease", first.GetName(), "Name")
	c.Eq("openrouter/x/glm", first.GetModel(), "Model")
	c.Eq("brent", first.GetOwner(), "Owner")
	c.Eq(int32(7), first.GetTurns(), "Turns")
	c.Eq(0.0123, first.GetCostUsd(), "CostUsd")
	c.Eq("conv-0", first.GetParentConversationId(), "ParentConversationId")
	c.Eq(int32(3), first.GetChildCount(), "ChildCount")
	c.Eq("c_1", first.GetChildId(), "ChildId")
	c.Eq("worker", first.GetChildName(), "ChildName")
	c.Eq("fundi", first.GetKind(), "Kind")
	c.Eq(rafikiv1.ConversationState_CONVERSATION_STATE_EXITED, first.GetState(), "State")
	c.Require().True(first.GetClosedAt() != nil, "a closed row must set closed_at")
	c.Eq(closed.Unix(), first.GetClosedAt().AsTime().Unix(), "ClosedAt")
	second := rows[1]
	c.False(second.ClosedAt != nil, "an open row must leave closed_at unset, got %v", second.GetClosedAt())
	c.Eq(rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT, second.GetState(), "second State")
	c.Eq("next-1", resp.Msg.GetNextCursor(), "NextCursor")
}

// TestListConversationsSortDefaultsToRecent pins both zero conventions: an
// UNSPECIFIED sort and an explicit RECENT reach the seam as the recent order.
func TestListConversationsSortDefaultsToRecent(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		sort rafikiv1.ConversationSort
		want connectapi.ConversationListSort
	}{
		{"unspecified", rafikiv1.ConversationSort_CONVERSATION_SORT_UNSPECIFIED, connectapi.ConversationListSortRecent},
		{"recent", rafikiv1.ConversationSort_CONVERSATION_SORT_RECENT, connectapi.ConversationListSortRecent},
		{"created", rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED, connectapi.ConversationListSortCreated},
		{"relevance", rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE, connectapi.ConversationListSortRelevance},
	} {
		f := &fakeConversationInsights{}
		_, err := newConversationsServer(f).ListConversations(context.Background(),
			connect.NewRequest(&rafikiv1.ListConversationsRequest{Sort: tc.sort}))
		c.Require().NoError(err, "%s: ListConversations", tc.name)
		c.Eq(tc.want, f.gotList.Sort, "%s: Sort", tc.name)
	}
}

// TestListConversationsStatesRoundTrip pins every named state's round trip;
// UNSPECIFIED inside the list is refused before the seam runs.
func TestListConversationsStatesRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{}
	_, err := newConversationsServer(f).ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{States: []rafikiv1.ConversationState{
			rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
			rafikiv1.ConversationState_CONVERSATION_STATE_EXITED,
			rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED,
			rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT,
		}}))
	c.Require().NoError(err, "ListConversations")
	c.EqDeep([]connectapi.ConversationListState{
		connectapi.ConversationListStateOpen,
		connectapi.ConversationListStateExited,
		connectapi.ConversationListStateClosed,
		connectapi.ConversationListStateNoAgent,
	}, f.gotList.States, "States")
}

// TestListConversationsRefusesNegativeLimit: a negative limit is a caller
// mistake, refused before the seam runs.
func TestListConversationsRefusesNegativeLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{}
	_, err := newConversationsServer(f).ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{Limit: -1}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "negative limit err = %v, want", err)
	c.False(f.listCalled, "the seam must not be called")
}

// The server clamps a caller's limit; it never forwards an unbounded one.
func TestListConversationsClampsLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{}
	_, err := newConversationsServer(f).ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{Limit: 100000}))
	c.Require().NoError(err, "ListConversations")
	c.Eq(500, f.gotList.Limit, "Limit")
}

// TestListConversationsRefusesUnspecifiedState: UNSPECIFIED inside states is
// not a state, and dropping it would silently widen the answer.
func TestListConversationsRefusesUnspecifiedState(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{}
	_, err := newConversationsServer(f).ListConversations(context.Background(),
		connect.NewRequest(&rafikiv1.ListConversationsRequest{States: []rafikiv1.ConversationState{
			rafikiv1.ConversationState_CONVERSATION_STATE_OPEN,
			rafikiv1.ConversationState_CONVERSATION_STATE_UNSPECIFIED,
		}}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "unspecified state err = %v, want", err)
	c.False(f.listCalled, "the seam must not be called")
}

// An out-of-range Timestamp is refused InvalidArgument before the seam runs.
func TestListConversationsRejectsOutOfRangeTimes(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.ListConversationsRequest
	}{
		{"since", &rafikiv1.ListConversationsRequest{Since: outOfRangeTimestamp}},
		{"until", &rafikiv1.ListConversationsRequest{Until: outOfRangeTimestamp}},
	} {
		f := &fakeConversationInsights{}
		_, err := newConversationsServer(f).ListConversations(context.Background(), connect.NewRequest(tc.req))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", tc.name)
		c.False(f.listCalled, "%s: the seam must not be called", tc.name)
	}
}

// TestListConversationsKeepsAdapterErrors pins the error contract the daemon
// adapter is built on: whatever error the seam returns reaches the caller
// unchanged, so the codes the adapter chose for the three listing sentinels
// -- invalid_argument for a bad cursor or a relevance sort without a query,
// failed_precondition for content search this database cannot serve -- arrive
// with their own code and message, and nothing is re-wrapped into internal.
func TestListConversationsKeepsAdapterErrors(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		// What ErrBadCursor and ErrRelevanceNeedsQuery become at the adapter.
		{"bad cursor", connect.NewError(connect.CodeInvalidArgument,
			errors.New("insights: cursor does not match this request")), connect.CodeInvalidArgument},
		{"relevance needs query", connect.NewError(connect.CodeInvalidArgument,
			errors.New("insights: relevance sort requires a query")), connect.CodeInvalidArgument},
		// What ErrContentSearchUnavailable becomes at the adapter.
		{"content search unavailable", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("insights: content search is unavailable on this database")), connect.CodeFailedPrecondition},
	} {
		s := newConversationsServer(&fakeConversationInsights{err: tc.err})
		_, err := s.ListConversations(context.Background(),
			connect.NewRequest(&rafikiv1.ListConversationsRequest{}))
		c.Eq(tc.want, connect.CodeOf(err), "%s: err = %v, want code", tc.name, err)
		c.Eq(tc.err.Error(), err.Error(), "%s: message", tc.name)
	}
}
