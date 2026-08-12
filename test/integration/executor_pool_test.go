// Package integration_test contains end-to-end tests for the executor pool.
//
// Build tag: integration
//
// These tests require a running daemon binary with an executor listener.
// The listener is not yet implemented (plan-07 scope); these tests become
// active when RAFIKI_EXECUTOR_LISTEN is wired in main.go.
//
// Run with:
//
//	set -a; . ./.env; set +a
//	go test ./test/integration/ -tags integration -count=1 -v -run TestExecutorPool
package integration_test

import (
	"testing"
)

// TestExecutorPool_FullLifecycle covers: enroll → dial in → selected by label →
// runs a child's bash → relabelled without restart → no longer matches → drop
// the connection → background job output survives the reconnect.
//
// Requires a running daemon with an executor listener. Skipped until plan-07
// adds the listener (RAFIKI_EXECUTOR_LISTEN).
func TestExecutorPool_FullLifecycle(t *testing.T) {
	t.Skip("executor listener not yet implemented (plan-07 scope)")
}

// TestExecutorPool_Narrowing covers: a child cannot reach an executor its parent
// could not, driven over the wire with a forged executorSelector.
//
// Requires a running daemon with enrolled executors. Skipped until plan-07
// adds the executor enrollment and listener.
func TestExecutorPool_Narrowing(t *testing.T) {
	t.Skip("executor enrollment not yet implemented (plan-07 scope)")
}
