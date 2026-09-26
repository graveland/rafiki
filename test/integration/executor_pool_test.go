// Package integration_test — end-to-end tests for the executor pool.
//
// These drive the reverse-dial transport the way a real deployment does: a
// daemon with an executor listener, an executor process that dials IN over TLS,
// enrolls with a one-time token, and stays live in the pool. That is the same
// path a laptop executor takes to a daemon it cannot be reached from.
//
// Run with:
//
//	set -a; . ./.env; set +a
//	go test ./test/integration/ -count=1 -v -run TestExecutorPool
package integration_test

import (
	"strings"
	"testing"
)

// TestExecutorPool_FullLifecycle covers the join over a real reverse-dialled
// connection: mint a token → the executor process dials IN over TLS → enrolls →
// becomes live in the pool → is SELECTED BY LABEL for a child, which is placed
// on it.
//
// That last step is the one this was missing. Selection was previously asserted
// only through refusal messages, which cannot tell you a child landed on the
// right machine — and the helper that identifies the enrolled executor was
// matching on labels, so against a test database with rows from earlier runs it
// happily compared against a stale one.
//
// Relabelling and revocation reaching a LIVE connection are unit-tested in
// pkg/execpool (refresh_test.go), where healthInterval is a field and the same
// assertions take milliseconds instead of two health ticks.
func TestExecutorPool_FullLifecycle(t *testing.T) {
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)

	execID := g.enrollExecutor(t, map[string]string{"env": "home", "tier": "cheap"})
	g.waitForLiveExecutors(t, 1)

	// Selected by label: a spawn naming env=home lands on it.
	childID, err := g.grantSpawn(t, "", "env=home", "anthropic/claude-x")
	if err != nil {
		t.Fatalf("spawn onto the enrolled executor failed: %v", err)
	}
	if placed := g.executorOf(t, childID); placed != execID {
		t.Fatalf("child placed on executor %q, want the one we enrolled (%s)", placed, execID)
	}

	// A selector matching nothing is refused, and the refusal counts the pool —
	// which is also how we know the executor is still live after the spawn.
	_, err = g.grantSpawn(t, "", "env=nowhere", "anthropic/claude-x")
	if err == nil || !strings.Contains(err.Error(), "1 live executor(s)") {
		t.Errorf("expected a refusal naming 1 live executor, got: %v", err)
	}

}

// executorOf returns the executor id a child was placed on, from its auto-label.
func (g *grantDaemon) executorOf(t *testing.T, childID string) string {
	t.Helper()
	return getChild(t, g.control(t), childID).GetLabels()["rafiki/executor"]
}
