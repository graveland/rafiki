// SPDX-License-Identifier: Apache-2.0

package tui

import (
	tea "charm.land/bubbletea/v2"

	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// railNodes is the rows the rail DRAWS: the tree with collapsed subtrees
// folded away. Everything that moves a cursor over the rail, sizes it or
// draws it reads this; anything acting on agents (stop, close, subagent
// counts) reads rail.Nodes(), since a hidden agent is still an agent.
func (c *Cockpit) railNodes() []rail.Node {
	return rail.Fold(c.rail.Nodes(), c.collapsed)
}

func (c *Cockpit) setCollapsed(id string, v bool) {
	if v {
		if c.collapsed == nil {
			c.collapsed = make(map[string]bool)
		}
		c.collapsed[id] = true
		return
	}
	delete(c.collapsed, id)
}

// reveal expands every ancestor of id, so a jump to a child hidden under a
// fold (an attention hop, a freshly spawned grandchild) lands on a row that is
// actually drawn.
func (c *Cockpit) reveal(id string) {
	for _, a := range c.rail.Ancestors(id) {
		c.setCollapsed(a, false)
	}
}

// expandSelected opens the folded row under the cursor, one level: rows
// beneath it keep their own fold state.
func (c *Cockpit) expandSelected() {
	c.setCollapsed(c.selected, false)
}

// collapseSelected folds the row under the cursor when it has descendants and
// is open; otherwise it moves the cursor to the parent, so repeated presses
// climb the tree the way they do in a file browser.
func (c *Cockpit) collapseSelected() tea.Cmd {
	rows := c.railNodes()
	for i, n := range rows {
		if n.ChildID != c.selected {
			continue
		}
		if !n.Folded && i+1 < len(rows) && rows[i+1].Depth > n.Depth {
			c.setCollapsed(n.ChildID, true)
			return nil
		}
		if n.Depth == 0 {
			return nil
		}
		for j := i - 1; j >= 0; j-- {
			if rows[j].Depth < n.Depth {
				c.selected = rows[j].ChildID
				c.selectedSeq++
				return c.previewCmd(c.selected)
			}
		}
	}
	return nil
}

// railWindow scrolls the rail: given rows total, the cursor's row index, the
// previous top and the pane height, it returns the first drawn row and how
// many rows are drawn. When everything fits it draws everything; otherwise it
// reserves two lines for the "more above/below" markers (constant, so rows do
// not shift as the markers come and go) and moves top only as far as it takes
// to keep the cursor in view.
func railWindow(rows, cursor, top, height int) (int, int) {
	if rows <= height {
		return 0, rows
	}
	vis := max(height-2, 1)
	if cursor >= 0 {
		if cursor < top {
			top = cursor
		}
		if cursor >= top+vis {
			top = cursor - vis + 1
		}
	}
	return max(min(top, rows-vis), 0), vis
}

// railMarker is one of the two reserved scroll lines: how many rows are cut off
// in that direction, or an empty line when none are.
func railMarker(arrow string, n int) string {
	if n == 0 {
		return "\n"
	}
	return styleRailDim.Render(arrow+" "+itoa(int64(n))+" more") + "\n"
}

// railPage is how far PgUp/PgDn move the rail cursor: one screenful of rows,
// less the two reserved marker lines and one row of overlap.
func (c *Cockpit) railPage() int {
	return max(c.bodyHeight()-3, 1)
}
