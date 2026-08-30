// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

// TestPaneStateIsPerChild pins the fix for C1b's shared-renderer finding: one
// renderer served every child, so hopping had to reset it, and a hop mid-stream
// painted the previous child's live tail into the new child's pane.
func TestPaneStateIsPerChild(t *testing.T) {
	c := &Cockpit{panes: map[string]*paneState{}}

	a := c.pane("c_a")
	b := c.pane("c_b")

	if a == b {
		t.Fatal("two children share one paneState")
	}
	if a.renderer == b.renderer {
		t.Fatal("two children share one renderer — this is the C1b bug")
	}
	if got := c.pane("c_a"); got != a {
		t.Error("pane() did not return the same paneState for the same child")
	}
}

// TestPaneEvictionFollowsSessions: pane state is view state for a session, so
// it must not outlive one. maxSessions bounds memory; panes must obey it too.
func TestPaneEvictionFollowsSessions(t *testing.T) {
	c := &Cockpit{panes: map[string]*paneState{}}
	c.pane("c_gone")

	c.evictPane("c_gone")

	if _, ok := c.panes["c_gone"]; ok {
		t.Error("evictPane left the paneState behind")
	}
}

// ── scrollback ───────────────────────────────────────────────────────────────

func manyLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line"
	}
	return out
}

// TestViewportFollowsWhenAtBottom: the common case is watching a live agent, so
// new output must keep the pane pinned.
func TestViewportFollowsWhenAtBottom(t *testing.T) {
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")

	c.syncViewport(p, manyLines(50))
	if !p.vp.AtBottom() {
		t.Fatal("a fresh pane must start at the bottom")
	}

	c.syncViewport(p, manyLines(60))
	if !p.vp.AtBottom() {
		t.Error("new content unpinned a pane that was at the bottom")
	}
}

// TestViewportHoldsPositionWhenScrolledUp: reading back through a transcript
// must not be yanked away by an agent still producing output. Being pulled to
// the bottom mid-read is worse than missing the newest line — the footer's
// "↓ more below" marker says there is more.
func TestViewportHoldsPositionWhenScrolledUp(t *testing.T) {
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")
	c.syncViewport(p, manyLines(100))

	p.vp.ScrollUp(20)
	before := p.vp.YOffset()
	if p.vp.AtBottom() {
		t.Fatal("fixture is wrong: still at the bottom after scrolling up")
	}

	c.syncViewport(p, manyLines(110))

	if got := p.vp.YOffset(); got != before {
		t.Errorf("YOffset moved from %d to %d while scrolled up", before, got)
	}
}

// TestSyncViewportTracksAtBottomForTheFooter: the "↓ more below" marker is the
// only thing telling you output is arriving off-screen.
func TestSyncViewportTracksAtBottomForTheFooter(t *testing.T) {
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")

	c.syncViewport(p, manyLines(100))
	if !p.atBottom {
		t.Error("atBottom false while pinned to the bottom")
	}

	p.vp.ScrollUp(30)
	c.syncViewport(p, manyLines(100))
	if p.atBottom {
		t.Error("atBottom stayed true after scrolling up; the footer marker would never appear")
	}
}

// TestViewportSoftWrapsLongLines: SoftWrap defaults to FALSE, which truncates
// a long line and hides the rest behind horizontal scrolling. An assistant's
// prose paragraph is ONE content line, so without this it is readable only by
// scrolling sideways.
func TestViewportSoftWrapsLongLines(t *testing.T) {
	c := newTestCockpit("c_a")
	c.width, c.height = 40, 20
	p := c.pane("c_a")

	if !p.vp.SoftWrap {
		t.Fatal("viewport must soft-wrap: a transcript is prose, not a table")
	}
}
