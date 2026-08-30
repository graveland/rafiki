// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/tui/rail"
)

func TestGlyphCoversEveryStatus(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"spawning", "◌"},
		{"idle", "○"},
		{"streaming", "◐"},
		{"tool_running", "⚒"},
		{"compacting", "⊛"},
		{"blocked_ui", "‼"},
		{"shutting_down", "◇"},
	} {
		if got := rail.Glyph(rail.Node{Status: tc.status}); got != tc.want {
			t.Errorf("Glyph(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// Every live status must have its own glyph: two statuses sharing one is a
// rail that cannot distinguish states the daemon distinguishes.
func TestEveryLiveStatusHasADistinctGlyph(t *testing.T) {
	seen := map[string]string{}
	for _, st := range rail.LiveStatuses() {
		g := rail.Glyph(rail.Node{Status: st})
		if g == "·" {
			t.Errorf("status %q falls through to the unknown glyph", st)
		}
		if prev, dup := seen[g]; dup {
			t.Errorf("statuses %q and %q share glyph %q", prev, st, g)
		}
		seen[g] = st
	}
}

func TestGlyphExitCodeDecidesTheMark(t *testing.T) {
	zero, one := int32(0), int32(1)
	if got := rail.Glyph(rail.Node{Exited: true, ExitCode: &zero}); got != "✓" {
		t.Errorf("clean exit = %q, want ✓", got)
	}
	if got := rail.Glyph(rail.Node{Exited: true, ExitCode: &one}); got != "✗" {
		t.Errorf("nonzero exit = %q, want ✗", got)
	}
	// A signalled child has NO exit code. ChildExited.exit_code is optional
	// precisely so that stays distinguishable from a clean 0.
	if got := rail.Glyph(rail.Node{Exited: true, ExitCode: nil}); got != "✗" {
		t.Errorf("signalled exit = %q, want ✗ -- absent is not success", got)
	}
}

func TestRetryingBeatsTheStatusGlyph(t *testing.T) {
	// An agent stuck in a retry loop is otherwise pixel-identical to one
	// working: both sit at "streaming".
	if got := rail.Glyph(rail.Node{Status: "streaming", Retrying: true}); got != "⟳" {
		t.Errorf("retrying = %q, want ⟳", got)
	}
}

func TestExitBeatsRetrying(t *testing.T) {
	code := int32(0)
	if got := rail.Glyph(rail.Node{Exited: true, ExitCode: &code, Retrying: true}); got != "✓" {
		t.Errorf("exited+retrying = %q, want ✓ -- a dead agent is not retrying", got)
	}
}

func TestGlyphOfAnUnknownStatusIsNeverEmpty(t *testing.T) {
	if got := rail.Glyph(rail.Node{Status: "teleporting"}); got == "" {
		t.Error("an unrecognised status must still render a glyph, not a hole in the rail")
	}
}
