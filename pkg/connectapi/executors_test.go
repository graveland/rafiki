// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type fakeExecutorLister struct {
	rows []ExecutorRow
	err  error
}

func (f fakeExecutorLister) ListExecutors(ctx context.Context, kind string) ([]ExecutorRow, error) {
	return f.rows, f.err
}

func TestListExecutorsMapsRows(t *testing.T) {
	s := NewServer(nil)
	s.SetExecutorLister(fakeExecutorLister{rows: []ExecutorRow{
		{ID: "exec-1", Machine: "greyshift", Eligible: true, LaunchKinds: []string{"claude"}},
		{ID: "exec-2", Machine: "silvershift", Eligible: false, Reason: "does not support launching \"claude\" children"},
	}})
	resp, err := s.ListExecutors(context.Background(), connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: "claude"}))
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.Msg.GetRows()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].GetMachine() != "greyshift" || !rows[0].GetEligible() {
		t.Errorf("row 0 mismatched: %+v", rows[0])
	}
	if rows[1].GetReason() == "" {
		t.Error("row 1 should carry its exclusion reason")
	}
}

func TestListExecutorsUnwiredIsUnavailable(t *testing.T) {
	s := NewServer(nil)
	_, err := s.ListExecutors(context.Background(), connect.NewRequest(&rafikiv1.ListExecutorsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if f.sawSelector != "env=work" || f.sawLimit != 7 {
		t.Errorf("seam saw (%q, %d), want the request's selector and limit", f.sawSelector, f.sawLimit)
	}
	rows := resp.Msg.GetRows()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].GetId() != "exec-1" || rows[0].GetMachine() != "greyshift" || !rows[0].GetEnabled() {
		t.Errorf("row 0 mismatched: %+v", rows[0])
	}
	for i, row := range rows {
		if row.GetEligible() {
			t.Errorf("row %d set eligible on the plain listing", i)
		}
		if row.GetReason() != "" {
			t.Errorf("row %d set reason %q on the plain listing", i, row.GetReason())
		}
		if row.GetId() == sentinelRow.ID {
			t.Error("the kind-scoped lister answered an empty-kind request")
		}
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
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", err)
	}
}

// TestListExecutorsUnwiredListerIsUnavailableForKind is the mirror pin: a
// non-empty kind still requires the kind-scoped lister, even with the admin
// wired.
func TestListExecutorsUnwiredListerIsUnavailableForKind(t *testing.T) {
	s := NewServer(nil)
	s.SetExecutorAdmin(&fakeExecutorAdmin{})
	_, err := s.ListExecutors(context.Background(),
		connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: "claude"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", err)
	}
}
