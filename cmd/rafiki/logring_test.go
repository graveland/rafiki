// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// Nothing the cockpit's own process logs may reach stderr while the alt screen
// is up. Capture rather than throttle: at warn level the executor's park and
// reconnect chatter still lands on the screen.
func TestLogRingCapturesInsteadOfPrinting(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newLogRing(4)
	l := slog.New(r)
	l.Info("execpool: executor joined", "id", "e1")

	got := r.Records()
	c.Require().Len(got, 1, "got %d records, want 1", len(got))
	c.StrContains(got[0], "executor joined", "record does not carry the message")
	c.StrContains(got[0], "e1", "record dropped its attributes")
}

// Bounded. The ring is what the future log pane reads; it must not grow
// without limit across a long session.
func TestLogRingDropsOldestPastCapacity(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newLogRing(3)
	l := slog.New(r)
	for _, m := range []string{"one", "two", "three", "four"} {
		l.Info(m)
	}
	got := r.Records()
	c.Require().Len(got, 3, "got %d records, want 3", len(got))
	c.NotStrContains(strings.Join(got, "\n"), "one", "the oldest record was not evicted")
	c.StrContains(strings.Join(got, "\n"), "four", "the newest record is missing")
}

// Info is kept. Nothing reaches the screen, so there is no reason to throttle,
// and an executor reconnect is only debuggable with the info-level trail.
func TestLogRingKeepsInfo(t *testing.T) {
	r := newLogRing(4)
	assert.NewCollecting(t).True(r.Enabled(context.Background(), slog.LevelInfo), "info records are dropped; the ring exists so they need not be")
}
