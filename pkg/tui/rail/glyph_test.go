// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/tui/rail"

	"github.com/multigres/testkit/assert"
)

func TestGlyphCoversEveryStatus(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"spawning", "◌"},
		{"idle", "○"},
		{"streaming", "◐"},
		{"tool_running", "⚒"},
		{"compacting", "⊛"},
		{"batch_wait", "⧖"},
		{"blocked_ui", "‼"},
		{"shutting_down", "◇"},
	} {
		got := rail.Glyph(rail.Node{Status: tc.status})
		assert.NewCollecting(t).Eq(tc.want, got, "Glyph(%q) = %q, want", tc.status, got)
	}
}

// Every live status must have its own glyph: two statuses sharing one is a
// rail that cannot distinguish states the daemon distinguishes.
func TestEveryLiveStatusHasADistinctGlyph(t *testing.T) {
	c := assert.NewCollecting(t)
	seen := map[string]string{}
	for _, st := range rail.LiveStatuses() {
		g := rail.Glyph(rail.Node{Status: st})
		c.NotEq("·", g, "status %q falls through to the unknown glyph", st)
		prev, dup := seen[g]
		c.False(dup, "statuses %q and %q share glyph %q", prev, st, g)
		seen[g] = st
	}
}

func TestGlyphExitCodeDecidesTheMark(t *testing.T) {
	c := assert.NewCollecting(t)
	zero, one := int32(0), int32(1)
	c.Eq("✓", rail.Glyph(rail.Node{Exited: true, ExitCode: &zero}), "clean exit")
	c.Eq("✗", rail.Glyph(rail.Node{Exited: true, ExitCode: &one}), "nonzero exit")
	// A signalled child has NO exit code. ChildExited.exit_code is optional
	// precisely so that stays distinguishable from a clean 0.
	c.Eq("✗", rail.Glyph(rail.Node{Exited: true, ExitCode: nil}), "signalled exit")
}

func TestRetryingBeatsTheStatusGlyph(t *testing.T) {
	// An agent stuck in a retry loop is otherwise pixel-identical to one
	// working: both sit at "streaming".
	assert.NewCollecting(t).Eq("⟳", rail.Glyph(rail.Node{Status: "streaming", Retrying: true}), "retrying")
}

func TestExitBeatsRetrying(t *testing.T) {
	code := int32(0)
	assert.NewCollecting(t).Eq("✓", rail.Glyph(rail.Node{Exited: true, ExitCode: &code, Retrying: true}), "exited+retrying")
}

func TestGlyphOfAnUnknownStatusIsNeverEmpty(t *testing.T) {
	assert.NewCollecting(t).NotEq("", rail.Glyph(rail.Node{Status: "teleporting"}), "an unrecognised status must still render a glyph, not a hole in the rail")
}

func TestAnimatedGlyphSpinsOnlyWhileWorking(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, status := range []string{"streaming", "tool_running", "compacting"} {
		n := rail.Node{Status: status}
		f0, f1 := rail.AnimatedGlyph(n, 0), rail.AnimatedGlyph(n, 1)
		c.NotEq(f1, f0, "status %q: AnimatedGlyph did not change between ticks (%q both)", status, f0)
		c.NotEq(rail.Glyph(n), f0, "status %q: AnimatedGlyph(tick=0) = %q, same as the static Glyph -- "+
			"want the spinner's own first frame", status, f0)
	}
}

func TestAnimatedGlyphIsStaticForNonWorkingStatuses(t *testing.T) {
	for _, status := range []string{"spawning", "idle", "blocked_ui", "shutting_down"} {
		n := rail.Node{Status: status}
		got, want := rail.AnimatedGlyph(n, 3), rail.Glyph(n)
		assert.NewCollecting(t).Eq(want, got, "status %q: AnimatedGlyph = %q, want the static Glyph", status, got)
	}
}

// Exit and retry must keep winning over a busy status, exactly as Glyph
// already decides -- a signalled-but-still-"streaming" row must not spin.
func TestAnimatedGlyphNeverSpinsAnExitedOrRetryingRow(t *testing.T) {
	code := int32(0)
	exited := rail.Node{Status: "streaming", Exited: true, ExitCode: &code}
	if got, want := rail.AnimatedGlyph(exited, 5), rail.Glyph(exited); got != want {
		t.Errorf("exited row spun: AnimatedGlyph = %q, want static %q", got, want)
	}
	retrying := rail.Node{Status: "streaming", Retrying: true}
	got, want := rail.AnimatedGlyph(retrying, 5), rail.Glyph(retrying)
	assert.NewCollecting(t).Eq(want, got, "retrying row spun: AnimatedGlyph")
}

func TestSpinnerFrameLoops(t *testing.T) {
	c := assert.NewCollecting(t)
	c.NotEq(rail.SpinnerFrame(1), rail.SpinnerFrame(0), "consecutive frames must differ")
	const frameCount = 10 // len(spinnerFrames) in glyph.go
	got := rail.SpinnerFrame(0)
	c.Eq(rail.SpinnerFrame(frameCount), got, "SpinnerFrame did not loop after %d frames: %q !=", frameCount, got)
}

// TestLiveStatusesIncludesBatchWait pins batch_wait's membership in the
// closed live-status set: without it, a parked child vanishes from any rail
// filter built on that list (the exact silent-empty bug the set is closed to
// prevent).
func TestLiveStatusesIncludesBatchWait(t *testing.T) {
	for _, st := range rail.LiveStatuses() {
		if st == "batch_wait" {
			return
		}
	}
	t.Errorf("LiveStatuses = %v, want it to include batch_wait", rail.LiveStatuses())
}

// TestWorkingExcludesBatchWait pins that a parked child does not spin:
// batch_wait is waiting on hours-scale work already submitted to the provider
// Batch API, not mid-turn computation the spinner stands for.
func TestWorkingExcludesBatchWait(t *testing.T) {
	assert.NewCollecting(t).False(rail.Working("batch_wait"), `Working("batch_wait") = true, want false -- a parked child must not spin`)
}

// TestGlyphBatchWaitIsStaticHourglass pins the parked glyph: the static
// hourglass ⧖ (U+29D6, single column), and AnimatedGlyph must return it
// unchanged at every tick of a full spinner cycle -- never a spinner frame.
func TestGlyphBatchWaitIsStaticHourglass(t *testing.T) {
	c := assert.NewAborting(t)
	n := rail.Node{Status: "batch_wait"}
	c.Eq("⧖", rail.Glyph(n), "Glyph(batch_wait)")
	for tick := 0; tick < 10; tick++ {
		got := rail.AnimatedGlyph(n, tick)
		c.Eq("⧖", got, "AnimatedGlyph(batch_wait, tick=%d) = %q, want the static ⧖", tick, got)
	}
}
