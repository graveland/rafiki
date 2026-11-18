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
	"testing"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)

	execID := g.enrollExecutor(t, map[string]string{"env": "home", "tier": "cheap"})
	g.waitForLiveExecutors(t, 1)

	// Selected by label: a spawn naming env=home lands on it.
	childID, err := g.grantSpawn(t, "", "env=home", "anthropic/claude-x")
	c.Require().NoError(err, "spawn onto the enrolled executor failed")
	c.Require().Eq(execID, g.executorOf(t, childID), "child placed on executor")

	// A selector matching nothing is REFUSED at spawn, because the child's set
	// is frozen from what the selector matches and would be empty. The refusal
	// carries selection's own "1 live executor(s)" text, which is also how we
	// know the executor is still live after the spawn. The count is asserted
	// exactly, so a wrong count still fails.
	resp, err := g.grantSpawnResp(t, "", "env=nowhere", "anthropic/claude-x")
	c.Require().Error(err, "a selector matching nothing must be refused, not spawn a child with an empty set")
	c.Require().True(spawnReportsLiveCount(resp.GetWarnings(), err, 1),
		"the refusal must carry selection's live count (1 live executor(s)), got %v", err)
}

// executorOf returns the executor id a child was placed on, from its auto-label.
func (g *grantDaemon) executorOf(t *testing.T, childID string) string {
	t.Helper()
	return getChild(t, g.control(t), childID).GetLabels()["rafiki/executor"]
}
