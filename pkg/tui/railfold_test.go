// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

func foldCockpit() *Cockpit {
	c := newTestCockpit("c_a")
	kid := summaryFor("c_b", "bravo", 0)
	kid.Labels[rail.ParentLabel] = "c_a"
	grand := summaryFor("c_c", "charlie", 0)
	grand.Labels[rail.ParentLabel] = "c_b"
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_a", "alpha", 0), kid, grand, summaryFor("c_d", "delta", 0)})
	c.selected = "c_a"
	c.focus = focusRail
	return c
}

func visibleIDs(c *Cockpit) []string {
	var out []string
	for _, n := range c.railNodes() {
		out = append(out, n.ChildID)
	}
	return out
}

func TestLeftRightFoldAndUnfoldTheSelectedRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := foldCockpit()

	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.EqDeep([]string{"c_a", "c_d"}, visibleIDs(c), "after ←")
	ck.Eq("c_a", c.selected, "cursor stays on the folded row")

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	ck.Eq("c_d", c.selected, "↓ skips the hidden rows")

	c.selected = "c_a"
	c.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	ck.EqDeep([]string{"c_a", "c_b", "c_c", "c_d"}, visibleIDs(c), "after →")
}

func TestLeftOnAnOpenLeafClimbsToTheParent(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := foldCockpit()
	c.selected = "c_c"

	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.Eq("c_b", c.selected, "leaf → parent")
	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // c_b has a kid: folds
	ck.Eq("c_b", c.selected, "open parent folds first")
	c.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	ck.Eq("c_a", c.selected, "folded row → its parent")
}

func TestHopIntoAFoldedSubtreeRevealsIt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := foldCockpit()
	c.setCollapsed("c_a", true)
	c.setCollapsed("c_b", true)

	c.hop("c_c")

	ck.EqDeep([]string{"c_a", "c_b", "c_c", "c_d"}, visibleIDs(c), "rows after hop")
}

func TestFoldedRowBadgeSumsHiddenAttention(t *testing.T) {
	c := foldCockpit()
	c.rail.Apply(turnEndFor("c_c", 3))
	c.setCollapsed("c_a", true)

	got := renderRail(c.railNodes(), "c_d", "c_a", 30, true, nil, 0)

	ck := assert.NewCollecting(t)
	ck.True(strings.Contains(got, "+"), "fold marker")
	ck.True(strings.Contains(got, "●1"), "grandchild's unread on the folded root")
}

func TestRailWindowKeepsTheCursorInViewAndReservesMarkerRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	top, vis := railWindow(5, 4, 0, 10)
	ck.Eq(0, top, "fits: top")
	ck.Eq(5, vis, "fits: all drawn")

	top, vis = railWindow(20, 0, 0, 10)
	ck.Eq(8, vis, "overflow reserves two lines")
	ck.Eq(0, top, "cursor at head")

	top, _ = railWindow(20, 12, top, 10)
	ck.Eq(5, top, "scrolls just far enough to show row 12")

	top, _ = railWindow(20, 6, top, 10)
	ck.Eq(5, top, "cursor already visible: no move")

	top, _ = railWindow(20, 19, top, 10)
	ck.Eq(12, top, "bottom clamps")
}

func TestTallRailScrollsToTheCursor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_00")
	var sums []*rafikiv1.ChildSummary
	for i := 0; i < 40; i++ {
		id := "c_" + strconv.Itoa(100+i)
		sums = append(sums, summaryFor(id, id, 0))
	}
	c.rail.Seed(sums)
	c.width, c.height, c.ready = 100, 20, true
	c.focus = focusRail
	c.selected = "c_139"

	view := c.View().Content
	ck.True(strings.Contains(view, "c_139"), "the selected row must be drawn")
	ck.True(strings.Contains(view, "↑"), "rows are cut off above")
}

func TestPagingKeysTraverseTheRail(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_100")
	var sums []*rafikiv1.ChildSummary
	for i := 0; i < 40; i++ {
		id := "c_" + strconv.Itoa(100+i)
		sums = append(sums, summaryFor(id, id, 0))
	}
	c.rail.Seed(sums)
	c.width, c.height, c.ready = 100, 20, true
	c.focus = focusRail
	c.selected = "c_100"

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	ck.Eq("c_"+strconv.Itoa(100+c.railPage()), c.selected, "pgdown moves a page")
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	ck.Eq("c_139", c.selected, "end")
	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	ck.Eq("c_"+strconv.Itoa(139-c.railPage()), c.selected, "pgup moves a page")
	c.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	ck.Eq("c_100", c.selected, "home")
}
