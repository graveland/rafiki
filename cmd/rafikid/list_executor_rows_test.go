package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestListExecutorRowsMarksLaunchEligibility(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, "", "claude"),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""),
	)
	rows, err := c.ListExecutorRows(context.Background(), "claude", "", "")
	ck.Require().NoError(err)
	ck.Require().Len(rows, 2, "want 2 rows, got %d", len(rows))
	byID := map[string]bool{}
	for _, r := range rows {
		byID[r.ID] = r.Eligible
	}
	ck.False(!byID["exec-1"], "exec-1 supports launching claude and should be eligible")
	ck.False(byID["exec-2"], "exec-2 does not support launching claude and should not be eligible")
}

func TestListExecutorRowsFundiKindIgnoresLaunchSupport(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, ""),
	)
	rows, err := c.ListExecutorRows(context.Background(), protocol.KindFundi, "", "")
	ck.NoError(err)
	ck.False(len(rows) != 1 || !rows[0].Eligible, "a fundi listing must not require launch-kind support: %+v", rows)
}

func TestListExecutorRowsRespectsAdmission(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift"}, "owner=someone-else", "claude"),
	)
	rows, err := c.ListExecutorRows(context.Background(), "claude", "brent", "")
	ck.Require().NoError(err)
	ck.Require().False(len(rows) != 1 || rows[0].Eligible, "owner mismatch must be ineligible: %+v", rows)
	ck.NotEq("", rows[0].Reason, "an ineligible row must carry a reason")
}
