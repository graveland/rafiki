// SPDX-License-Identifier: Apache-2.0

package rail

// spinnerFrames is the shared "something is happening" animation -- one
// braille-dot cycle used for every working status. The rail's AnimatedGlyph
// and the transcript's tail indicator (pkg/tui) both index into it with the
// same tick counter, so the two spinners move in lock-step.
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// SpinnerFrame returns the animation frame for tick.
func SpinnerFrame(tick int) string {
	return spinnerFrames[tick%len(spinnerFrames)]
}

// AnimatedGlyph is Glyph, except a working row (Working(n.Status)) cycles
// through SpinnerFrame instead of sitting on its static icon. Exited and
// retrying rows never spin -- Glyph's own precedence (exit beats status, retry
// beats a live status) already decided those are not "making progress" glyphs.
//
// This is deliberately a SEPARATE function from Glyph: Glyph stays exactly
// what railWidthFor measures, so the glyph column's width is fixed regardless
// of animation state and never triggers a rail-width recalculation.
func AnimatedGlyph(n Node, tick int) string {
	if !n.Exited && !n.Retrying && Working(n.Status) {
		return SpinnerFrame(tick)
	}
	return Glyph(n)
}

// Glyph is the activity indicator for one row: what this agent is doing right
// now. It is NOT attention -- a working agent shows a glyph and no badge.
//
// Exit wins over status, and retry wins over both live statuses, because an
// agent stuck retrying sits at "streaming" and is otherwise indistinguishable
// from one making progress.
func Glyph(n Node) string {
	if n.Exited {
		if n.ExitCode != nil && *n.ExitCode == 0 {
			return "✓"
		}
		// Absent is not success: a signalled child has no exit code at all,
		// which is why ChildExited.exit_code is optional.
		return "✗"
	}
	if n.Retrying {
		return "⟳"
	}
	switch n.Status {
	case "spawning":
		return "◌"
	case "idle":
		return "○"
	case "streaming":
		return "◐"
	case "tool_running":
		return "⚒"
	case "compacting":
		return "⊛"
	case "blocked_ui":
		return "‼"
	case "shutting_down":
		return "◇"
	case "exited":
		return "✗"
	default:
		// protocol.Status is a closed set of eight, but it is a STRING on the
		// wire so a newer daemon can add one without every client being
		// regenerated. An unknown value must render something rather than
		// leaving a hole in the rail.
		return "·"
	}
}
