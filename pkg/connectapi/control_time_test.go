// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// outOfRangeDuration is a Duration a wire client can send whose seconds
// overflow the valid range, so CheckValid refuses it.
var outOfRangeDuration = &durationpb.Duration{Seconds: 1 << 62}

// outOfRangeTimestamp is the Timestamp twin: seconds far past 9999-12-31.
var outOfRangeTimestamp = &timestamppb.Timestamp{Seconds: 1 << 62}

// ─── Kill timeouts (Duration) ────────────────────────────────────────────────

// A round-trip: explicit shutdown/kill spans reach the lifecycle seam unchanged.
func TestKillTimeoutsRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	f := &fakeLifecycle{}
	s.SetChildLifecycle(f)

	_, err := s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId:         "c_1",
		ShutdownTimeout: durationpb.New(7 * time.Second),
		KillTimeout:     durationpb.New(2 * time.Second),
	}))
	c.Require().NoError(err, "Kill")
	c.Eq(7*time.Second, f.gotShutdow, "shutdown timeout reached the seam")
	c.Eq(2*time.Second, f.gotKill, "kill timeout reached the seam")
}

// Unset OR an explicit zero Duration reaches the seam as 0, which the daemon
// reads as its 180s/30s defaults (TestKillTimeoutDefaults) -- exactly the old
// shutdown_timeout_ms = 0.
func TestKillUnsetOrZeroTimeoutsAreZero(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.KillRequest
	}{
		{"unset", &rafikiv1.KillRequest{ChildId: "c_1"}},
		{"explicit zero", &rafikiv1.KillRequest{ChildId: "c_1",
			ShutdownTimeout: durationpb.New(0), KillTimeout: durationpb.New(0)}},
	} {
		s := connectapi.NewServer(nil)
		f := &fakeLifecycle{}
		s.SetChildLifecycle(f)
		_, err := s.Kill(context.Background(), connect.NewRequest(tc.req))
		c.Require().NoError(err, "%s: Kill", tc.name)
		c.Eq(time.Duration(0), f.gotShutdow, "%s: shutdown timeout", tc.name)
		c.Eq(time.Duration(0), f.gotKill, "%s: kill timeout", tc.name)
	}
}

// An out-of-range Duration is refused InvalidArgument before the seam is
// touched, per design rule 4.
func TestKillRejectsOutOfRangeTimeouts(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.KillRequest
	}{
		{"shutdown", &rafikiv1.KillRequest{ChildId: "c_1", ShutdownTimeout: outOfRangeDuration}},
		{"kill", &rafikiv1.KillRequest{ChildId: "c_1", KillTimeout: outOfRangeDuration}},
	} {
		s := connectapi.NewServer(nil)
		f := &fakeLifecycle{}
		s.SetChildLifecycle(f)
		_, err := s.Kill(context.Background(), connect.NewRequest(tc.req))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", tc.name)
		c.Eq("", f.killedID, "%s: seam was touched", tc.name)
	}
}

// ─── CloseAllExited.older_than (Duration) ────────────────────────────────────

func TestCloseAllExitedOlderThanRoundTrips(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{}
	_, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{OlderThan: durationpb.New(90 * time.Second)}))
	c.Require().NoError(err, "CloseAllExited")
	c.Eq(90*time.Second, f.exitedOlder, "older_than reached the seam")
}

// Unset OR an explicit zero Duration keeps the old older_than_ms = 0 meaning
// ("all exited entries"): the seam sees 0.
func TestCloseAllExitedUnsetOrZeroOlderThanIsZero(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.CloseAllExitedRequest
	}{
		{"unset", &rafikiv1.CloseAllExitedRequest{}},
		{"explicit zero", &rafikiv1.CloseAllExitedRequest{OlderThan: durationpb.New(0)}},
	} {
		f := &fakeChildOps{}
		_, err := newChildOpsServer(f).CloseAllExited(context.Background(), connect.NewRequest(tc.req))
		c.Require().NoError(err, "%s: CloseAllExited", tc.name)
		c.Eq(time.Duration(0), f.exitedOlder, "%s: older_than", tc.name)
	}
}

func TestCloseAllExitedRejectsOutOfRangeOlderThan(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{}
	_, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{OlderThan: outOfRangeDuration}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(time.Duration(0), f.exitedOlder, "seam was touched")
}

// ─── ListChildren.since (Timestamp) ──────────────────────────────────────────

func TestListChildrenSinceRoundTrips(t *testing.T) {
	c := assert.NewCollecting(t)
	since := time.UnixMilli(150)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{
		{ChildID: "c_old", StartedAt: time.UnixMilli(100)},
		{ChildID: "c_new", StartedAt: time.UnixMilli(200)},
	}})
	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Since: timestamppb.New(since)}))
	c.Require().NoError(err, "ListChildren")
	c.Require().Len(resp.Msg.GetChildren(), 1, "since=150ms must keep only the newer child")
	c.Eq("c_new", resp.Msg.GetChildren()[0].GetChildId(), "kept child")
}

// Unset OR an epoch Timestamp means unbounded: every child comes back, the old
// since = 0.
func TestListChildrenUnsetOrEpochSinceIsUnbounded(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.ListChildrenRequest
	}{
		{"unset", &rafikiv1.ListChildrenRequest{}},
		{"epoch", &rafikiv1.ListChildrenRequest{Since: timestamppb.New(time.UnixMilli(0))}},
	} {
		s := connectapi.NewServer(nil)
		s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{
			{ChildID: "c_1", StartedAt: time.UnixMilli(100)},
			{ChildID: "c_2", StartedAt: time.UnixMilli(200)},
		}})
		resp, err := s.ListChildren(context.Background(), connect.NewRequest(tc.req))
		c.Require().NoError(err, "%s: ListChildren", tc.name)
		c.Len(resp.Msg.GetChildren(), 2, "%s: an unbounded since must keep every child", tc.name)
	}
}

func TestListChildrenRejectsOutOfRangeSince(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{ChildID: "c_1"}}})
	_, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Since: outOfRangeTimestamp}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// ─── Search session filter .since (Timestamp) ────────────────────────────────

func TestSearchRejectsOutOfRangeSessionSince(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{}
	_, err := newChildOpsServer(f).Search(context.Background(),
		connect.NewRequest(&rafikiv1.SearchRequest{
			Query: "needle",
			SessionFilter: &rafikiv1.SearchRequest_SearchSessionFilter{
				Since: outOfRangeTimestamp,
			},
		}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Nil(f.searchReq, "seam was touched")
}

// ─── EnrollExecutor.ttl (Duration) ───────────────────────────────────────────

func TestEnrollExecutorRejectsOutOfRangeTTL(t *testing.T) {
	c := assert.NewCollecting(t)
	// No executor admin is wired: a valid request would be Unavailable, so
	// InvalidArgument proves the range check runs first, per design rule 4.
	s := connectapi.NewServer(nil)
	_, err := s.EnrollExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.EnrollExecutorRequest{Ttl: outOfRangeDuration}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// ─── EventCursor.floor (Timestamp) ───────────────────────────────────────────

func TestStreamEventsRejectsOutOfRangeFloor(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := setupStreamServer(t, &fakeLineage{}, eventlog.NewMemory(), &fakeSource{})
	stream, err := client.StreamEvents(ctx,
		connect.NewRequest(&rafikiv1.StreamEventsRequest{
			Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
			Cursor:  &rafikiv1.EventCursor{Floor: outOfRangeTimestamp},
		}))
	if err == nil {
		c.False(stream.Receive(), "expected error on an out-of-range floor")
		err = stream.Err()
	}
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}
