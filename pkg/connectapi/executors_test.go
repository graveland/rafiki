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
