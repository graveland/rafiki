package main

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// statusCell is the list STATUS cell: a machine status is carried through
// unchanged EXCEPT an idle child whose display state refines it into
// waiting:<reason> or failed. Every other status keeps its granularity.
func TestStatusCell(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		cs   *rafikiv1.ChildSummary
		want string
	}{
		{"idle", &rafikiv1.ChildSummary{Status: "idle"}, "idle"},
		{"idle waiting on operator", &rafikiv1.ChildSummary{Status: "idle", DisplayState: "waiting", WaitingOn: "operator"}, "waiting:operator"},
		{"idle waiting on a job", &rafikiv1.ChildSummary{Status: "idle", DisplayState: "waiting", WaitingOn: "job"}, "waiting:job"},
		{"idle failed", &rafikiv1.ChildSummary{Status: "idle", DisplayState: "failed"}, "failed"},
		{"streaming keeps its granularity", &rafikiv1.ChildSummary{Status: "streaming", DisplayState: "waiting", WaitingOn: "operator"}, "streaming"},
		{"exited", &rafikiv1.ChildSummary{Status: "exited"}, "exited"},
	} {
		c.Eq(tc.want, statusCell(tc.cs), tc.name)
	}
}

// The list table renders the cell through formatChildStatus, so a waiting idle
// row shows waiting:<reason> there too, not just in statusCell in isolation.
func TestStatusCellFlowsIntoTheListCell(t *testing.T) {
	c := assert.NewCollecting(t)
	waiting := &rafikiv1.ChildSummary{Status: "idle", DisplayState: "waiting", WaitingOn: "operator"}
	c.Eq("waiting:operator", formatChildStatus(waiting, false), "list STATUS cell")
	failed := &rafikiv1.ChildSummary{Status: "idle", DisplayState: "failed"}
	c.Eq("failed", formatChildStatus(failed, false), "list STATUS cell")
}
