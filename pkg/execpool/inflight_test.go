package execpool

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// captureLogs points slog at a buffer for the duration of the test and restores
// the previous logger at cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// warnCount counts the in-flight warning lines captured so far.
func warnCount(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), "in-flight calls past the warning threshold")
}

// TestInFlightWarnAtBelowStreamLimit ties the warning threshold to the stream
// limit: the threshold must sit below it so the warning is visible before
// callers start waiting, and the pairing cannot silently rot if either number
// moves.
func TestInFlightWarnAtBelowStreamLimit(t *testing.T) {
	assert.NewCollecting(t).Greater(inFlightWarnAt, maxExecutorStreams,
		"the HTTP/2 stream limit must sit above the in-flight warning threshold")
}

func TestInFlightWarnsOncePerCrossing(t *testing.T) {
	c := assert.NewCollecting(t)
	buf := captureLogs(t)

	f := &inFlight{id: "e1"}
	var release []func()
	for i := 0; i < inFlightWarnAt; i++ {
		release = append(release, f.enter())
	}
	c.Eq(1, warnCount(buf), "crossing the threshold must warn exactly once")

	// Pushing past the threshold without falling back below half must not warn
	// again: the flag stays set.
	for i := 0; i < 50; i++ {
		release = append(release, f.enter())
	}
	c.Eq(1, warnCount(buf), "250 in-flight calls must still warn only once")

	for _, r := range release {
		r()
	}
}

func TestInFlightRearmsAfterFallingBelowHalf(t *testing.T) {
	c := assert.NewCollecting(t)
	buf := captureLogs(t)

	f := &inFlight{id: "e1"}
	var release []func()
	for i := 0; i < inFlightWarnAt; i++ {
		release = append(release, f.enter())
	}
	c.Eq(1, warnCount(buf), "first crossing warns once")

	// Release down past half the threshold, re-arming the warning.
	for i := 0; i < 101; i++ {
		release[i]()
	}

	// Climb back to the threshold: the second crossing must warn again.
	for i := 0; i < 101; i++ {
		release = append(release, f.enter())
	}
	c.Eq(2, warnCount(buf), "a second upward crossing after re-arming must warn a second time")

	for _, r := range release[101:] {
		r()
	}
}

func TestInFlightNilIsNoop(t *testing.T) {
	c := assert.NewCollecting(t)
	var f *inFlight
	// Must not panic, and the returned release func must be callable.
	release := f.enter()
	release()
	c.Eq(0, warnCount(captureLogs(t)), "a nil counter logs nothing")
}
