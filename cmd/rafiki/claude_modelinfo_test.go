// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// unreachableDaemonProfile seeds an isolated profile manifest naming a socket
// nothing is listening on, so a dial through it fails deterministically even
// if a real daemon happens to be running.
func unreachableDaemonProfile(t *testing.T) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: filepath.Join(t.TempDir(), "no-such.sock")},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
}

// The behaviour that must never regress: an unreachable daemon returns 0,
// which leaves Claude Code's own compaction default alone and lets the session
// start. `rafiki claude` working with the daemon down is the difference
// between "the daemon is down" and "I cannot start a coding session".
//
// The catalog-dependent assertions (a known model reserves 5%-10%, an unknown
// model answers Known=false with zeroes) now live on the daemon side in
// cmd/rafikid/controller_modelinfo_test.go, because the client no longer reads
// the catalog itself — it asks the daemon over the ModelInfo RPC.
func TestAutoCompactWindowReturnsZeroWhenDaemonDown(t *testing.T) {
	unreachableDaemonProfile(t)

	got := claudeAutoCompactWindow(context.Background(), nil, "anthropic/claude-opus-5")
	assert.NewAborting(t).Eq(0, got, "an unreachable daemon must yield 0, got")
}

// Bounded: a slow lookup must not delay the launch. The whole RPC is raced
// against a budget; whatever replaces it must keep that property.
func TestAutoCompactWindowIsBounded(t *testing.T) {
	c := assert.NewAborting(t)
	unreachableDaemonProfile(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead
	start := time.Now()
	c.Eq(0, claudeAutoCompactWindow(ctx, nil, "anthropic/claude-opus-5"), "got")
	c.LessOrEqual(2*time.Second, time.Since(start), "took")
}
