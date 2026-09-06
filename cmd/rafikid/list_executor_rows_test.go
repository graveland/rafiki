package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestListExecutorRowsMarksLaunchEligibility(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, "", "claude"),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""),
	)
	rows, err := c.ListExecutorRows(context.Background(), "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	byID := map[string]bool{}
	for _, r := range rows {
		byID[r.ID] = r.Eligible
	}
	if !byID["exec-1"] {
		t.Error("exec-1 supports launching claude and should be eligible")
	}
	if byID["exec-2"] {
		t.Error("exec-2 does not support launching claude and should not be eligible")
	}
}

func TestListExecutorRowsFundiKindIgnoresLaunchSupport(t *testing.T) {
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, ""),
	)
	rows, err := c.ListExecutorRows(context.Background(), protocol.KindFundi, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Eligible {
		t.Fatalf("a fundi listing must not require launch-kind support: %+v", rows)
	}
}

func TestListExecutorRowsRespectsAdmission(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift"}, "owner=someone-else", "claude"),
	)
	rows, err := c.ListExecutorRows(context.Background(), "claude", "brent")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Eligible {
		t.Fatalf("owner mismatch must be ineligible: %+v", rows)
	}
	if rows[0].Reason == "" {
		t.Error("an ineligible row must carry a reason")
	}
}
