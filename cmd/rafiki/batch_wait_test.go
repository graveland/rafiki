package main

import "testing"

// TestColorStatusBatchWait pins batch_wait's list color. The brief asks for
// blue if a blue helper exists in output.go, else yellow — output.go has no
// blue helper (dim/red/green/yellow/cyan/magenta only), so batch_wait renders
// yellow, alongside shutting_down.
func TestColorStatusBatchWait(t *testing.T) {
	if got := colorStatus("batch_wait", false); got != "batch_wait" {
		t.Errorf("colorStatus(batch_wait, false) = %q, want the bare status", got)
	}
	if got, want := colorStatus("batch_wait", true), yellow("batch_wait"); got != want {
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
