// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The behaviour that must never regress: an unreachable daemon returns 0,
// which leaves Claude Code's own compaction default alone and lets the session
// start. `rafiki claude` working with the daemon down is the difference
// between "the daemon is down" and "I cannot start a coding session".
//
// The catalog-dependent assertions (a known model reserves 5%-10%, an unknown
// model answers Known=false with zeroes) now live on the daemon side in
// cmd/rafikid/controller_modelinfo_test.go, because the client no longer reads
// the catalog itself — it asks the daemon over ctrl_model_info.
func TestAutoCompactWindowReturnsZeroWhenDaemonDown(t *testing.T) {
	// Point both the remote-control URL and the UDS at nothing, so the dial
	// fails deterministically even if a real daemon happens to be running.
	t.Setenv("RAFIKI_CONTROL_URL", "")
	t.Setenv("RAFIKI_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))

	got := claudeAutoCompactWindow(context.Background(), "anthropic/claude-opus-5")
	if got != 0 {
		t.Fatalf("an unreachable daemon must yield 0, got %d", got)
	}
}

// Bounded: a slow lookup must not delay the launch. The whole RPC is raced
// against a budget; whatever replaces it must keep that property.
func TestAutoCompactWindowIsBounded(t *testing.T) {
	t.Setenv("RAFIKI_CONTROL_URL", "")
	t.Setenv("RAFIKI_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead
	start := time.Now()
	if got := claudeAutoCompactWindow(ctx, "anthropic/claude-opus-5"); got != 0 {
		t.Fatalf("got %d", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s; the lookup must never delay a launch", d)
	}
}
