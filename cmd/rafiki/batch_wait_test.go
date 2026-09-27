package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestColorStatusBatchWait pins batch_wait's list color. batch_wait renders
// blue (final review): it is long-lived (a batch can take hours) and must not
// collide with shutting_down's yellow — the brief's yellow fallback applied
// only while output.go had no blue helper.
func TestColorStatusBatchWait(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("batch_wait", colorStatus("batch_wait", false), "colorStatus(batch_wait, false)")
	got, want := colorStatus("batch_wait", true), blue("batch_wait")
	c.Eq(want, got, "colorStatus(batch_wait, true)")
}

// TestIsAttachableBatchWait pins that a parked child is attachable: it is
// running — its LLM call is parked in the provider Batch API — so the cockpit
// can usefully focus on it and watch for the batch result.
func TestIsAttachableBatchWait(t *testing.T) {
	ch := completionChild{ChildID: "c_batch", Status: "batch_wait"}
	assert.NewCollecting(t).True(isAttachable(ch), "isAttachable(batch_wait) = false, want true")
}
