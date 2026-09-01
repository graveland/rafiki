// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"strings"
	"testing"
)

// Nothing the cockpit's own process logs may reach stderr while the alt screen
// is up. Capture rather than throttle: at warn level the executor's park and
// reconnect chatter still lands on the screen.
func TestLogRingCapturesInsteadOfPrinting(t *testing.T) {
	r := newLogRing(4)
	l := slog.New(r)
	l.Info("execpool: executor joined", "id", "e1")

	got := r.Records()
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !strings.Contains(got[0], "executor joined") {
		t.Errorf("record does not carry the message: %q", got[0])
	}
	if !strings.Contains(got[0], "e1") {
		t.Errorf("record dropped its attributes: %q", got[0])
	}
}

// Bounded. The ring is what the future log pane reads; it must not grow
// without limit across a long session.
func TestLogRingDropsOldestPastCapacity(t *testing.T) {
	r := newLogRing(3)
	l := slog.New(r)
	for _, m := range []string{"one", "two", "three", "four"} {
		l.Info(m)
	}
	got := r.Records()
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if strings.Contains(strings.Join(got, "\n"), "one") {
		t.Error("the oldest record was not evicted")
	}
	if !strings.Contains(strings.Join(got, "\n"), "four") {
		t.Error("the newest record is missing")
	}
}

// Info is kept. Nothing reaches the screen, so there is no reason to throttle,
// and an executor reconnect is only debuggable with the info-level trail.
func TestLogRingKeepsInfo(t *testing.T) {
	r := newLogRing(4)
	if !r.Enabled(nil, slog.LevelInfo) {
		t.Error("info records are dropped; the ring exists so they need not be")
	}
}
