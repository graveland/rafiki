package main

import "testing"

// TestColorStatusBatchWait pins batch_wait's list color. batch_wait renders
// blue (final review): it is long-lived (a batch can take hours) and must not
// collide with shutting_down's yellow — the brief's yellow fallback applied
// only while output.go had no blue helper.
func TestColorStatusBatchWait(t *testing.T) {
	if got := colorStatus("batch_wait", false); got != "batch_wait" {
		t.Errorf("colorStatus(batch_wait, false) = %q, want the bare status", got)
	}
	if got, want := colorStatus("batch_wait", true), blue("batch_wait"); got != want {
		t.Errorf("colorStatus(batch_wait, true) = %q, want %q", got, want)
	}
}

// TestIsAttachableBatchWait pins that a parked child is attachable: it is
// running — its LLM call is parked in the provider Batch API — so the cockpit
// can usefully focus on it and watch for the batch result.
func TestIsAttachableBatchWait(t *testing.T) {
	ch := completionChild{ChildID: "c_batch", Status: "batch_wait"}
	if !isAttachable(ch) {
		t.Error("isAttachable(batch_wait) = false, want true")
	}
}
