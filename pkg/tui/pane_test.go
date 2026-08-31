// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

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
	const width = 40
	long := strings.Repeat("alpha beta ", 40)
	blocks := []session.Block{{Kind: session.KindUser, Final: true, Text: long}}

	lines := newRenderer().Lines(blocks, 1, width)
	if len(lines) < 5 {
		t.Fatalf("a %d-character line rendered to %d rows at width %d; it is not wrapping",
			len(long), len(lines), width)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(ansi.Strip(l)); w > width {
			t.Errorf("row %d is %d columns wide, over the %d available: %q", i, w, width, l)
		}
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

	if narrow <= wide {
		t.Errorf("rows at width 40 = %d, at width 120 = %d; the cache was not rewrapped",
			narrow, wide)
	}
}
