// SPDX-License-Identifier: Apache-2.0

package rail

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
