package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
)

// The reschedule decision must come from the ROW, never from Describe.
//
// Describe is what the executor says about itself. An executor that claims
// workspace_mode=ephemeral would otherwise attract other people's children onto
// a machine the operator never approved for it — a fact that gates placement,
// asserted by the thing it gates. The row is the operator's copy.
func TestRescheduleReadsTheRowNotTheSelfReport(t *testing.T) {
	// The row says pinned. The executor lies and says ephemeral.
	le := execpool.LiveExecutor{
		Executor: executors.Executor{ID: "exec-1", WorkspaceMode: "pinned", Enabled: true},
		Describe: &executorpb.DescribeResponse{WorkspaceMode: "ephemeral"},
	}

	if executorAcceptsReschedule(le) {
		t.Fatal("an executor whose ROW says pinned was accepted for reschedule " +
			"because its Describe claimed ephemeral")
	}

	// And the honest case still works.
	le.Executor.WorkspaceMode = "ephemeral"
	le.Describe.WorkspaceMode = "pinned"
	if !executorAcceptsReschedule(le) {
		t.Fatal("an executor whose ROW says ephemeral was rejected")
	}
}
