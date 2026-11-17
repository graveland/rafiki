// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

var (
	styleRailFocused = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleRailBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	styleRailDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// fmtCost renders a spend, converted through cur when set. Zero renders as
// nothing: a row of "$0.00" beside every idle agent is noise, and the number
// is only interesting once there is one. This is the rail's own zero rule
// layered over costfmt.Format, which otherwise renders zero as "-" (the
// right call for a data table, the wrong one for a live status rail).
func fmtCost(usd float64, cur *clientstate.Currency) string {
	if usd == 0 {
		return ""
	}
	return costfmt.Format(usd, cur)
}

// kindTag is the rail's one-character kind marker. Empty for fundi, which is
// the default kind and would otherwise pay a column on every row to say so.
func kindTag(kind string) string {
	switch kind {
	case "claude":
		return " ᶜ"
	case "pi":
		return " ᵖ"
	default:
		return ""
	}
}

// nativeTag marks a synthesized Task subagent. Distinct from kindTag: both a
// real claude child and a native subagent are kind "claude", and only the
// former is a budgeted cross-process agent.
func nativeTag(n rail.Node) string {
	if n.Native {
		return "~"
	}
	return ""
}

// kidsTag renders a row's direct-child count as " (N)". Empty for a leaf: the
// absence of children is not a number worth a column.
func kidsTag(n rail.Node) string {
	if n.Kids == 0 {
		return ""
	}
	return " (" + strconv.Itoa(n.Kids) + ")"
}

// renderRail draws the tree.
//
// It renders whatever rows it is given. The "stay hidden below two rows" look
// used to be an early return here (`len(nodes) < 2` → ""), but a renderer that
// suppresses on its own cannot be overridden: an explicit ^R has to be able to
// reveal a one-row rail as a peek — and with one agent the rail is also the
// only path to the spawn form (`n` is a rail-local key), so suppressing it
// here made both keys read as dead. The default look is railCols' decision
// now: it hides the rail below two rows unless railPeek outranks it, so a
// fresh `create` still shows a full-width conversation and the rail still
// grows out of a normal session — no cockpit to configure, no empty pane to
// look at. Only an empty node list renders nothing, because there is no such
// thing as a zero-row rail to peek at.
//
// Rows are clipped BEFORE styling, so the width budget is measured on plain
// text and lipgloss escape sequences never count against it.
// railWidthFor sizes the rail to its CONTENT, clamped.
//
// Sizing to content rather than to the window is the point. The rail used to be
// a fixed 22 columns and truncated real names; making it a fraction of the
// window would fix that and reintroduce what the fixed width was chosen to
// avoid — a rail that resizes as you drag makes the conversation reflow on
// every frame of the drag. Content changes only when a child spawns, exits or
// is renamed, so this re-wraps about as often as the rail itself changes.
//
// The clamp matters in both directions: railMin keeps a two-agent cockpit from
// a sliver, and railMaxFrac stops one absurdly-named agent eating the
// transcript. The budget counts everything renderRail puts in the plain row --
// cursor, indent, glyph, name, native tag, kind tag, kid count, badge and cost,
// because those are what get clipped.
func railWidthFor(nodes []rail.Node, total int, cur *clientstate.Currency) int {
	want := railMin
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.ChildID
		}
		kids := kidsTag(n)
		w := 2 + 2*n.Depth + ansi.StringWidth(rail.Glyph(n)) + 1 + ansi.StringWidth(name) + ansi.StringWidth(nativeTag(n)) + ansi.StringWidth(kindTag(n.Kind)) + ansi.StringWidth(kids)
		if n.Attention > 0 {
			w += 2 + len(strconv.Itoa(n.Attention))
		}
		if n.Folded {
			w++
		}
		if cost := fmtCost(n.TotalCost(), cur); cost != "" {
			w += 1 + len(cost)
		}
		if w > want {
			want = w
		}
	}
	if cap := total * railMaxPct / 100; cap > railMin && want > cap {
		want = cap
	}
	return want
}

func renderRail(nodes []rail.Node, focused, selected string, width int, paneFocused bool, cur *clientstate.Currency, tick int) string {
	if len(nodes) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.ChildID
		}
		badge := ""
		if n.Attention > 0 {
			badge = " ●" + strconv.Itoa(n.Attention)
		}
		// The cursor is part of the PLAIN row so it counts against the width
		// budget like everything else. Every row carries the two columns,
		// selected or not, so rows do not shift as the cursor moves.
		// The cursor thickens when the rail HOLDS focus, so the pane that is
		// taking your arrow keys is identifiable without pressing one.
		cursor := "  "
		if n.ChildID == selected {
			cursor = "▸ "
			if paneFocused {
				cursor = "▶ "
			}
		}
		// The cost joins the PLAIN row so it counts against the width budget,
		// like the cursor and the badge. Rows are clipped before styling, so
		// anything appended afterwards escapes the pane and bleeds colour into
		// the transcript.
		fold := ""
		if n.Folded {
			fold = "+"
		}
		left := cursor + strings.Repeat("  ", n.Depth) + fold + rail.AnimatedGlyph(n, tick) + " " + nativeTag(n) + name + kindTag(n.Kind) + kidsTag(n) + badge
		cost := fmtCost(n.TotalCost(), cur)
		row := clip(left, width)
		if cost != "" {
			if gap := width - ansi.StringWidth(clip(left, width-len(cost)-1)) - len(cost); gap >= 1 {
				row = clip(left, width-len(cost)-1) + strings.Repeat(" ", gap) + cost
			}
		}
		switch {
		case n.ChildID == focused:
			sb.WriteString(styleRailFocused.Render(row))
		case n.Attention > 0:
			sb.WriteString(styleRailBadge.Render(row))
		case n.Exited:
			sb.WriteString(styleRailDim.Render(row))
		default:
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// searchName is a row's display name: the conversation's name, then the
// linked agent's, then the id's first eight characters. An anonymous
// conversation still has an id, so the last fallback always lands.
func searchName(l *rafikiv1.ConversationListing) string {
	if l.GetName() != "" {
		return l.GetName()
	}
	if l.GetChildName() != "" {
		return l.GetChildName()
	}
	id := l.GetId()
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// searchKidsTag renders a row's child count as " (N)" from the listing, the
// same shape the agent rail's kidsTag renders from the tree. Empty for a
// leaf: the absence of children is not a number worth a column.
func searchKidsTag(l *rafikiv1.ConversationListing) string {
	if l.GetChildCount() == 0 {
		return ""
	}
	return " (" + strconv.Itoa(int(l.GetChildCount())) + ")"
}

// searchFold is a row's fold mark: + when it has children and is collapsed,
// - when it is expanded, nothing for a leaf. It sits where the agent rail's
// fold mark does, one column before the name. The expanded state lives on the
// model, which bakes it into the row (searchRow.folded, set by rebuild), so
// the render never reaches into the model's maps.
func searchFold(r searchRow) string {
	if r.l.GetChildCount() == 0 {
		return ""
	}
	if r.folded {
		return "+"
	}
	return "-"
}

// searchStateTag marks the one state that changes what a row can do: a
// conversation whose linked agent is open carries a live marker, because
// committing to it lands in that agent's transcript, live. Every other state
// renders nothing: closed rows are dimmed instead, and an exited agent's
// affordance is the resume on commit, not a glyph.
func searchStateTag(l *rafikiv1.ConversationListing) string {
	if l.GetState() == rafikiv1.ConversationState_CONVERSATION_STATE_OPEN {
		return " ●"
	}
	return ""
}

// searchSortName is the right-aligned label on the query line: the sort the
// rail is currently ordered by, in the short lowercase form. UNSPECIFIED
// means RECENT on the wire, so the label says recent rather than showing a
// spelling nobody chose.
func searchSortName(sort rafikiv1.ConversationSort) string {
	switch sort {
	case rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED:
		return "created"
	case rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE:
		return "best match"
	default:
		return "recent"
	}
}

// searchQueryLine is the pane's first line: "/ " and the query as typed, the
// sort name right-aligned. The query clips to leave the sort its column; the
// sort wins because the query is the thing growing under the user's fingers
// and the sort is the thing that tells them what order the result is in.
func searchQueryLine(query, sortName string, width int) string {
	right := ""
	if sortName != "" {
		right = " " + sortName
	}
	left := clip("/ "+query, max(width-ansi.StringWidth(right), 1))
	return padTo(left, max(width-ansi.StringWidth(right), 1)) + right
}

// searchRowStyle picks a row's style. Closed conversations read as history
// rather than as live fleet members, which is the same dim the agent rail
// uses for exited rows.
func searchRowStyle(l *rafikiv1.ConversationListing) lipgloss.Style {
	if l.GetState() == rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED {
		return styleRailDim
	}
	return lipgloss.Style{}
}

// searchRailWidth sizes the search pane to its content, exactly as
// railWidthFor sizes the agent rail: the query line and the widest row set
// the want, railMin keeps a sliver from happening, and the same railMaxPct
// clamp stops one long name eating the transcript. The budget counts
// everything the render puts in the plain row (cursor, indent, fold, name,
// kid count, state tag), because those are what get clipped.
func searchRailWidth(rows []searchRow, query, sortName string, total int) int {
	want := railMin
	if q := 2 + ansi.StringWidth(query) + ansi.StringWidth(" "+sortName); q > want {
		want = q
	}
	for _, r := range rows {
		w := 2 + 2*r.depth + ansi.StringWidth(searchFold(r)) + 1 +
			ansi.StringWidth(searchName(r.l)) +
			ansi.StringWidth(searchKidsTag(r.l)) +
			ansi.StringWidth(searchStateTag(r.l))
		if w > want {
			want = w
		}
	}
	if cap := total * railMaxPct / 100; cap > railMin && want > cap {
		want = cap
	}
	return want
}

// renderSearchRail draws the search pane: the query line first, then one row
// per conversation, expanded children inline at their depth. Rows are
// clipped BEFORE styling, so the width budget is measured on plain text and
// lipgloss escape sequences never count against it.
func renderSearchRail(rows []searchRow, query, sortName, selected string, width int, paneFocused bool) string {
	var sb strings.Builder
	sb.WriteString(searchQueryLine(query, sortName, width))
	sb.WriteString("\n")
	for _, r := range rows {
		id := r.l.GetId()
		cursor := "  "
		if id == selected {
			cursor = "▸ "
			if paneFocused {
				cursor = "▶ "
			}
		}
		fold := searchFold(r)
		left := cursor + strings.Repeat("  ", r.depth) + fold + " " + searchName(r.l) +
			searchKidsTag(r.l) + searchStateTag(r.l)
		row := clip(left, width)
		sb.WriteString(searchRowStyle(r.l).Render(row))
		sb.WriteString("\n")
	}
	return sb.String()
}

// clip truncates s to width DISPLAY COLUMNS, appending an ellipsis when it cuts.
//
// Display columns, not runes: a lipgloss-styled string carries ANSI escape
// sequences that are runes but occupy no columns, and a CJK glyph is one rune
// occupying two. Counting runes made the conversation pane lose about a fifth
// of its width and amputate the trailing reset escape, bleeding colour into
// the rail. ansi.Truncate understands both and preserves the escapes it cuts
// across.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// padTo pads s with spaces to exactly width display columns, clipping if it is
// already wider. See clip for why display columns and not runes.
func padTo(s string, width int) string {
	w := ansi.StringWidth(s)
	if w >= width {
		return clip(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}
