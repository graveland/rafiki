// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeExecutorLister struct {
	rows []ExecutorRow
	err  error
}

func (f fakeExecutorLister) ListExecutors(ctx context.Context, kind string) ([]ExecutorRow, error) {
	return f.rows, f.err
}

func TestListExecutorsMapsRows(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	s.SetExecutorLister(fakeExecutorLister{rows: []ExecutorRow{
		{ID: "exec-1", Machine: "greyshift", Eligible: true, LaunchKinds: []string{"claude"}},
		{ID: "exec-2", Machine: "silvershift", Eligible: false, Reason: "does not support launching \"claude\" children"},
	}})
	resp, err := s.ListExecutors(context.Background(), connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: "claude"}))
	c.Require().NoError(err)
	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 2, "want 2 rows, got %d", len(rows))
	if rows[0].GetMachine() != "greyshift" || !rows[0].GetEligible() {
		t.Errorf("row 0 mismatched: %+v", rows[0])
	}
	c.NotEq("", rows[1].GetReason(), "row 1 should carry its exclusion reason")
}

// TestListExecutorsKindScopedTimestampsRideThrough pins the kind-scoped
// path's timestamp contract: the lister's rows map onto the wire AS-IS — a
// timestamp the lister set reaches the caller unchanged, and one it left
// unset stays 0 rather than sprouting a value. ListExecutorRows populates
// neither field today (a live-only row has no persisted sighting to show),
// so 0 is what the daemon's kind-scoped listing actually serves.
func TestListExecutorsKindScopedTimestampsRideThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	joined := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	seen := joined.Add(-time.Hour)
	s := NewServer(nil)
	s.SetExecutorLister(fakeExecutorLister{rows: []ExecutorRow{
		{ID: "exec-set", ConnectedAtMs: joined.UnixMilli(), LastSeenMs: seen.UnixMilli()},
		{ID: "exec-unset"},
	}})
	resp, err := s.ListExecutors(context.Background(),
		connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: "claude"}))
	c.Require().NoError(err)
	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 2, "want 2 rows, got %d", len(rows))
	if got, want := rows[0].GetConnectedAtMs(), joined.UnixMilli(); got != want {
		t.Errorf("row 0 connected_at_ms = %d, want the lister's %d", got, want)
	}
	got, want := rows[0].GetLastSeenMs(), seen.UnixMilli()
	c.Eq(want, got, "row 0 last_seen_ms")
	if rows[1].GetConnectedAtMs() != 0 || rows[1].GetLastSeenMs() != 0 {
		t.Errorf("row 1 = (connected_at_ms %d, last_seen_ms %d), want both 0",
			rows[1].GetConnectedAtMs(), rows[1].GetLastSeenMs())
	}
}

func TestListExecutorsUnwiredIsUnavailable(t *testing.T) {
	s := NewServer(nil)
	_, err := s.ListExecutors(context.Background(), connect.NewRequest(&rafikiv1.ListExecutorsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "want CodeUnavailable, got %v", err)
}

// sentinelRow marks the kind-scoped lister in the empty-kind tests: if the
// empty-kind path ever routed to the lister, this row would leak into the
// answer and the assertions below would catch it.
var sentinelRow = ExecutorRow{ID: "lister-saw-this-kind"}

// TestListExecutorsEmptyKindUsesAdminSeam pins the empty-kind contract: kind=""
// is the plain management listing, taken from the executor-admin seam with the
// request's selector and limit passed through, and the rows it returns keep
// eligible/reason unset — eligibility is NOT evaluated on this path.
func TestListExecutorsEmptyKindUsesAdminSeam(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	// The kind-scoped lister is wired with a sentinel row: it must NOT answer
	// this request even though it is available.
	s.SetExecutorLister(fakeExecutorLister{rows: []ExecutorRow{sentinelRow}})
	f := &fakeExecutorAdmin{listRows: []ExecutorRow{
		{ID: "exec-1", Machine: "greyshift", Enabled: true},
		{ID: "exec-2", Machine: "silvershift"},
	}}
	s.SetExecutorAdmin(f)

	resp, err := s.ListExecutors(context.Background(),
		connect.NewRequest(&rafikiv1.ListExecutorsRequest{Selector: "env=work", Limit: 7}))
	c.Require().NoError(err)
	if f.sawSelector != "env=work" || f.sawLimit != 7 {
		t.Errorf("seam saw (%q, %d), want the request's selector and limit", f.sawSelector, f.sawLimit)
	}
	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 2, "want 2 rows, got %d", len(rows))
	if rows[0].GetId() != "exec-1" || rows[0].GetMachine() != "greyshift" || !rows[0].GetEnabled() {
		t.Errorf("row 0 mismatched: %+v", rows[0])
	}
	for i, row := range rows {
		c.False(row.GetEligible(), "row %d set eligible on the plain listing", i)
		c.Eq("", row.GetReason(), "row %d set reason %q on the plain listing", i, row.GetReason())
		c.NotEq(sentinelRow.ID, row.GetId(), "the kind-scoped lister answered an empty-kind request")
	}
}

// TestListExecutorsEmptyKindNeedsAdminWired pins which seam serves which path:
// with the kind-scoped lister wired but the admin NOT, an empty-kind request
// is Unavailable rather than silently degrading into an eligibility-scoped
// listing.
func TestListExecutorsEmptyKindNeedsAdminWired(t *testing.T) {
	s := NewServer(nil)
	s.SetExecutorLister(fakeExecutorLister{rows: []ExecutorRow{sentinelRow}})
	_, err := s.ListExecutors(context.Background(),
		connect.NewRequest(&rafikiv1.ListExecutorsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "want CodeUnavailable, got %v", err)
}

// TestListExecutorsUnwiredListerIsUnavailableForKind is the mirror pin: a
// non-empty kind still requires the kind-scoped lister, even with the admin
// wired.
func TestListExecutorsUnwiredListerIsUnavailableForKind(t *testing.T) {
	s := NewServer(nil)
	s.SetExecutorAdmin(&fakeExecutorAdmin{})
	_, err := s.ListExecutors(context.Background(),
		connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: "claude"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "want CodeUnavailable, got %v", err)
}
