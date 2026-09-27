// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
)

// TestPaneStateIsPerChild pins the fix for C1b's shared-renderer finding: one
// renderer served every child, so hopping had to reset it, and a hop mid-stream
// painted the previous child's live tail into the new child's pane.
func TestPaneStateIsPerChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Cockpit{panes: map[string]*paneState{}}

	a := c.pane("c_a")
	b := c.pane("c_b")

	ck.Require().NotEq(b, a, "two children share one paneState")
	ck.Require().NotEq(b.renderer, a.renderer, "two children share one renderer — this is the C1b bug")
	ck.Eq(a, c.pane("c_a"), "pane() did not return the same paneState for the same child")
}

// TestPaneEvictionFollowsSessions: pane state is view state for a session, so
// it must not outlive one. maxSessions bounds memory; panes must obey it too.
func TestPaneEvictionFollowsSessions(t *testing.T) {
	c := &Cockpit{panes: map[string]*paneState{}}
	c.pane("c_gone")

	c.evictPane("c_gone")

	_, ok := c.panes["c_gone"]
	assert.NewCollecting(t).False(ok, "evictPane left the paneState behind")
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
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")

	c.syncViewport(p, manyLines(50))
	ck.Require().True(p.vp.AtBottom(), "a fresh pane must start at the bottom")

	c.syncViewport(p, manyLines(60))
	ck.True(p.vp.AtBottom(), "new content unpinned a pane that was at the bottom")
}

// TestViewportHoldsPositionWhenScrolledUp: reading back through a transcript
// must not be yanked away by an agent still producing output. Being pulled to
// the bottom mid-read is worse than missing the newest line — the footer's
// "↓ more below" marker says there is more.
func TestViewportHoldsPositionWhenScrolledUp(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")
	c.syncViewport(p, manyLines(100))

	p.vp.ScrollUp(20)
	before := p.vp.YOffset()
	ck.Require().False(p.vp.AtBottom(), "fixture is wrong: still at the bottom after scrolling up")

	c.syncViewport(p, manyLines(110))

	ck.Eq(before, p.vp.YOffset(), "YOffset moved from")
}

// TestSyncViewportTracksAtBottomForTheFooter: the "↓ more below" marker is the
// only thing telling you output is arriving off-screen.
func TestSyncViewportTracksAtBottomForTheFooter(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.width, c.height = 80, 20
	p := c.pane("c_a")

	c.syncViewport(p, manyLines(100))
	ck.True(p.atBottom, "atBottom false while pinned to the bottom")

	p.vp.ScrollUp(30)
	c.syncViewport(p, manyLines(100))
	ck.False(p.atBottom, "atBottom stayed true after scrolling up; the footer marker would never appear")
}

// A long line must WRAP, not disappear sideways: an assistant's prose
// paragraph is one content line, and unwrapped it is readable only by
// scrolling horizontally.
//
// This used to assert viewport.SoftWrap, which is off again — the viewport
// re-wraps every line on every Update AND every View (10.9ms and 10.6ms on a
// 6933-line transcript, against 2.4µs and 167µs without), so a held arrow key
// outran the screen. The renderer wraps instead. The MECHANISM changed and the
// requirement did not, so the test asserts the outcome rather than the flag.
func TestLongLinesWrapRatherThanRunOffTheEdge(t *testing.T) {
	c := assert.NewCollecting(t)
	const width = 40
	long := strings.Repeat("alpha beta ", 40)
	blocks := []session.Block{{Kind: session.KindUser, Final: true, Text: long}}

	lines := newRenderer().Lines(blocks, 1, width)
	c.Require().GreaterOrEqual(5, len(lines), "a %d-character line rendered to %d rows at width %d; it is not wrapping", len(long), len(lines), width)
	for i, l := range lines {
		w := ansi.StringWidth(ansi.Strip(l))
		c.LessOrEqual(width, w, "row %d is %d columns wide, over the %d available: %q", i, w, width, l)
	}
}

// Wrapping is the renderer's now, so a resize has to invalidate its cache —
// every cached line was wrapped to the old width.
func TestResizeRewrapsTheCachedTranscript(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindUser, Final: true, Text: strings.Repeat("alpha beta ", 40),
	}}
	r := newRenderer()
	narrow := len(r.Lines(blocks, 1, 40))
	wide := len(r.Lines(blocks, 1, 120))

	assert.NewCollecting(t).Greater(wide, narrow, "rows at width 40")
}
