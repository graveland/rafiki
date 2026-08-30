// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/session"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestCockpit(focus string) *Cockpit {
	return NewCockpit(Options{BaseURL: "http://127.0.0.1:1", ChildID: focus})
}

func summaryFor(id, name string, latest int32) *rafikiv1.ChildSummary {
	return &rafikiv1.ChildSummary{ChildId: id, Name: name, Status: "idle",
		Labels: map[string]string{}, LatestOrdinal: &latest}
}

func turnEndFor(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}}
}

func textEventFor(id, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}}}
}

// ── rail rendering ───────────────────────────────────────────────────────────

func TestRailHiddenForASingleChild(t *testing.T) {
	// Session-first: create/attach <id> shows no rail at all. The rail grows
	// out of a normal session -- no cockpit to configure, no empty pane.
	nodes := []rail.Node{{ChildID: "c_1", Name: "coordinator", Status: "idle"}}
	if got := renderRail(nodes, "c_1", "c_1", 24); got != "" {
		t.Errorf("renderRail with one child = %q, want empty", got)
	}
}

func TestRailAppearsWithTheSecondChild(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "coordinator", Status: "streaming"},
		{ChildID: "c_2", Name: "scout", ParentID: "c_1", Depth: 1, Status: "idle", Attention: 2},
	}
	got := renderRail(nodes, "c_1", "c_1", 24)
	if got == "" {
		t.Fatal("renderRail with two children must render")
	}
	for _, want := range []string{"coordinator", "scout", "2", rail.Glyph(nodes[0])} {
		if !strings.Contains(got, want) {
			t.Errorf("rail missing %q:\n%s", want, got)
		}
	}
}

func TestRailIndentsByDepth(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "root", Status: "idle"},
		{ChildID: "c_2", Name: "kid", ParentID: "c_1", Depth: 1, Status: "idle"},
		{ChildID: "c_3", Name: "grandkid", ParentID: "c_2", Depth: 2, Status: "idle"},
	}
	lines := strings.Split(strings.TrimRight(renderRail(nodes, "c_1", "c_1", 30), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("want 3 rows, got %d: %v", len(lines), lines)
	}
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	for i := 1; i < 3; i++ {
		if indent(lines[i]) <= indent(lines[i-1]) {
			t.Errorf("row %d must indent deeper than row %d:\n%v", i, i-1, lines)
		}
	}
}

func TestRailRowsAreClippedToWidth(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "a", Status: "idle"},
		{ChildID: "c_2", Name: strings.Repeat("verylongname", 20), Status: "idle"},
	}
	for _, line := range strings.Split(renderRail(nodes, "c_1", "c_1", 20), "\n") {
		if len([]rune(line)) > 20 {
			t.Errorf("row is %d runes, want <= 20: %q", len([]rune(line)), line)
		}
	}
}

func TestClipCountsRunesNotBytes(t *testing.T) {
	// A child name holds whatever a spawner typed. Byte truncation would split
	// a rune and corrupt the line. clip measures DISPLAY COLUMNS (a CJK glyph
	// is one rune but two columns) — see TestClipMeasuresDisplayWidthNotRunes
	// in railview_test.go for the fuller regression pinning this.
	got := clip("日本語のエージェント", 5)
	if w := ansi.StringWidth(got); w != 5 {
		t.Errorf("clip = %q (%d display columns), want 5", got, w)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clip = %q, want an ellipsis", got)
	}
}

// ── hop, LRU, keys ───────────────────────────────────────────────────────────

func TestHopRetainsTheOldTranscript(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})
	c.sessions["c_1"].Apply(textEventFor("c_1", "keep me"))

	c.hop("c_2")
	defer c.shutdown()

	if c.sessions["c_1"] == nil {
		t.Fatal("hopping away must KEEP the transcript: a full replay on every hop back is " +
			"the cost the cockpit exists to remove")
	}
	if len(c.sessions["c_1"].Blocks) != 1 {
		t.Errorf("blocks = %d, want 1", len(c.sessions["c_1"].Blocks))
	}
	if c.focused() != "c_2" {
		t.Errorf("focused = %q, want c_2", c.focused())
	}
}

func TestHopMarksTheOldChildRead(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})
	c.rail.SetFocus("c_1")

	ev := turnEndFor("c_1", 5)
	c.sessions["c_1"].Apply(ev)
	c.rail.Apply(ev)
	c.hop("c_2")
	defer c.shutdown()

	n, _ := c.rail.Get("c_1")
	if n.Attention != 0 || n.Seen != 5 {
		t.Errorf("c_1 = attention %d seen %d, want 0/5", n.Attention, n.Seen)
	}
}

func TestLRUEvictsTheOldestButNeverTheFocused(t *testing.T) {
	c := newTestCockpit("c_0")
	defer c.shutdown()
	for i := 1; i <= maxSessions+2; i++ {
		c.hop("c_" + strconv.Itoa(i))
	}
	if len(c.sessions) > maxSessions {
		t.Fatalf("sessions = %d, want at most %d", len(c.sessions), maxSessions)
	}
	if _, ok := c.sessions["c_0"]; ok {
		t.Error("the least-recently-focused session should have been evicted")
	}
	if _, ok := c.sessions[c.focused()]; !ok {
		t.Error("the FOCUSED session must never be evicted")
	}
}

func TestSendModesAreDistinctKeys(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want rafikiv1.SendMode
	}{
		{"enter", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{"alt+enter", rafikiv1.SendMode_SEND_MODE_STEER},
		{"ctrl+s", rafikiv1.SendMode_SEND_MODE_STEER},
		{"ctrl+x", rafikiv1.SendMode_SEND_MODE_ABORT},
		{"j", rafikiv1.SendMode_SEND_MODE_UNSPECIFIED},
	} {
		if got := modeForKey(tc.key); got != tc.want {
			t.Errorf("modeForKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
	// Inferring the mode from agent state removes a real choice: C1a-2 made a
	// prompt to a busy agent durably QUEUE, so queueing a follow-up and
	// interrupting the running turn are both things a user wants.
	if modeForKey("enter") == modeForKey("alt+enter") {
		t.Fatal("prompt and steer must not collapse onto one key")
	}
}

// child_spawned is the only event that introduces a rail row, and a child
// spawned during a disconnect has its child_spawned in the past -- the server
// replays only children named in the cursor. Without the self-heal that child
// is invisible for the rest of the session.
func TestTrafficFromAnUnknownChildTriggersAReseed(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})

	c.applyEvent(turnEndFor("c_ghost", 3))
	if !c.reseeding {
		t.Fatal("an event from an unknown child must request a re-seed")
	}

	c.reseeding = false
	c.applyEvent(turnEndFor("c_1", 4))
	if c.reseeding {
		t.Error("a known child must not trigger a re-seed")
	}
}

func TestNeighbourWrapsInDisplayOrder(t *testing.T) {
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.rail.SetFocus("c_a")
	if got := c.neighbour(-1); got != "c_b" {
		t.Errorf("neighbour(-1) from the first row = %q, want c_b (wrap)", got)
	}
	if got := c.neighbour(+1); got != "c_b" {
		t.Errorf("neighbour(+1) = %q, want c_b", got)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	c := newTestCockpit("c_1")
	c.shutdown()
	c.shutdown() // must not panic on a nil stop func
}

// ── regressions from the C1b whole-branch review ─────────────────────────────

// FINDING 1. seedMsg used to open the focus stream via hop(c.focused()), and
// hop refuses a move to the child already focused -- so the call was an
// unconditional no-op and `attach <id>` / `create` opened a cockpit whose only
// event source was the rail: six small types, no messages, no deltas, no
// history. A permanently empty pane.
func TestSeedOpensTheFocusStreamForTheInitialChild(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})

	if c.stopRail == nil {
		t.Error("seed must start the rail stream")
	}
	if c.stopFocus == nil {
		t.Fatal("seed must open a FOCUS stream for the initially focused child; " +
			"without it the pane only ever sees rail-tier events and stays empty")
	}
}

// FINDING 2. The rail subscription covers every child in the subject but
// carries none of their content. Applying it to a retained, non-focused session
// pushed that session's cursor past what it had rendered, and hop resumes from
// exactly that cursor -- so everything the agent produced while you were away
// was skipped, silently and permanently.
func TestRailEventsDoNotAdvanceANonFocusedSessionsCursor(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})

	// Focused on c_1, it reaches ordinal 10 through the focus stream.
	c.applyEvent(&rafikiv1.Event{ChildId: "c_1", Ordinal: ptr32(10),
		Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{}}})
	if got := c.sessions["c_1"].Cursor; got != 10 {
		t.Fatalf("focused cursor = %d, want 10", got)
	}

	// Hop away. c_1's session is retained; the rail keeps reporting its turns.
	c.hop("c_2")
	c.applyEvent(turnEndFor("c_1", 250))

	if got := c.sessions["c_1"].Cursor; got != 10 {
		t.Fatalf("non-focused cursor = %d, want 10 -- a rail event advanced it, so hopping "+
			"back would resume from %d and skip ordinals 11..%d forever", got, got, got)
	}
}

// FINDING 3. One renderer is shared by every session and its live-tail cache is
// keyed on a fingerprint, not on a child. The store used to be guarded by
// `if !needRender`, so a CHANGED tail computed a fresh string and then emitted
// the stale one -- the previous child's half-finished paragraph, for the whole
// of the next child's turn.
func TestRendererDoesNotBleedAcrossSessions(t *testing.T) {
	r := newRenderer()
	one := []session.Block{{Kind: session.KindAssistant, Text: "AAA-from-child-one"}}
	r.Lines(one, 0)
	r.Lines(one, 0) // settle the cache

	for _, tail := range []string{"BBB-1", "BBB-12", "BBB-123"} {
		two := []session.Block{{Kind: session.KindAssistant, Text: tail}}
		out := strings.Join(r.Lines(two, 0), "\n")
		if strings.Contains(out, "AAA-from-child-one") {
			t.Fatalf("render of %q leaked the previous child's tail:\n%s", tail, out)
		}
		if !strings.Contains(out, tail) {
			t.Errorf("render of %q did not contain it:\n%s", tail, out)
		}
	}
}

// FINDING 4. reseeding was set by applyEvent and cleared only when the RPC
// returned, and the eventMsg case dispatched on it every time -- so each event
// arriving during a slow ListChildren queued another concurrent one. The
// cockpit amplified against a daemon that was already slow, which is the exact
// condition the self-heal exists for.
func TestReseedDispatchesAtMostOneInFlight(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})

	dispatched := 0
	for i := int32(0); i < 5; i++ {
		_, cmd := c.Update(eventMsg{turnEndFor("c_ghost", i)})
		if cmd != nil {
			// tea.Batch always returns non-nil; count the re-seed explicitly.
			if c.reseedInFlight && !c.reseeding {
				dispatched++
				c.reseeding = false
			}
		}
	}
	if dispatched != 1 {
		t.Fatalf("dispatched %d re-seeds for a burst from one unknown child, want 1", dispatched)
	}

	// The reply releases the latch so a later gap can still self-heal.
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})
	if c.reseedInFlight {
		t.Error("seedMsg must clear reseedInFlight")
	}
}

// FINDING 5. ListChildrenRequest has no subject filter, so an unfiltered seed
// installed a row for every live child on the daemon. Rows outside the subject
// are frozen by construction -- their events never match -- so they keep their
// seed-time glyph forever, never badge, and still absorb focus.
func TestSeedIsNarrowedToTheSubject(t *testing.T) {
	kids := []*rafikiv1.ChildSummary{
		summaryFor("c_root", "root", 0),
		withParent(summaryFor("c_kid", "kid", 0), "c_root"),
		withParent(summaryFor("c_grandkid", "grandkid", 0), "c_kid"),
		summaryFor("c_other", "unrelated root", 0),
		withParent(summaryFor("c_otherkid", "unrelated kid", 0), "c_other"),
	}

	sub := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", ChildID: "c_root",
		Subject: &rafikiv1.EventSubject{
			Scope:       &rafikiv1.EventSubject_Subtree{Subtree: "c_root"},
			IncludeSelf: true,
		}})
	defer sub.shutdown()

	// Drive the real seed path rather than calling inSubject directly, so this
	// also fails if the narrowing is ever unwired from the handler.
	sub.Update(seedMsg{children: kids})
	got := map[string]bool{}
	for _, n := range sub.rail.Nodes() {
		got[n.ChildID] = true
	}
	for _, want := range []string{"c_root", "c_kid", "c_grandkid"} {
		if !got[want] {
			t.Errorf("%s missing from the subtree seed; got %v", want, got)
		}
	}
	for _, bad := range []string{"c_other", "c_otherkid"} {
		if got[bad] {
			t.Errorf("%s is outside the subscription but was seeded; its row would be frozen "+
				"at its seed-time status forever", bad)
		}
	}

	all := NewCockpit(Options{BaseURL: "http://127.0.0.1:1"})
	defer all.shutdown()
	all.Update(seedMsg{children: kids})
	if n := all.rail.Len(); n != len(kids) {
		t.Errorf("subject `all` seeded %d of %d children", n, len(kids))
	}
}

func ptr32(v int32) *int32 { return &v }

func withParent(s *rafikiv1.ChildSummary, parent string) *rafikiv1.ChildSummary {
	s.Labels[rail.ParentLabel] = parent
	return s
}

// ── rail selection ───────────────────────────────────────────────────────────

// TestMoveSelectionDoesNotHop pins the reason the rail has a cursor at all.
// hop opens a Connect subscription (openFocus -> streams.StartFocus), so the
// old move-and-hop binding churned one focus stream per keystroke: arrowing
// past five agents opened five. Browsing must move a cursor and nothing else;
// enter commits.
func TestMoveSelectionDoesNotHop(t *testing.T) {
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0),
		summaryFor("c_b", "bravo", 0),
		summaryFor("c_c", "charlie", 0),
	})
	c.rail.SetFocus("c_a")
	c.selected = "c_a"

	c.moveSelection(+1)

	if c.selected != "c_b" {
		t.Errorf("selection = %q, want c_b", c.selected)
	}
	if got := c.rail.Focus(); got != "c_a" {
		t.Errorf("moving the selection changed focus to %q; focus must only "+
			"change on commit", got)
	}
}

// TestMoveSelectionClampsAtTheEnds: selection clamps where neighbour() wraps.
// Two bindings that both wrap are indistinguishable in use, and wrapping is
// what the attention jump does.
func TestMoveSelectionClampsAtTheEnds(t *testing.T) {
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.selected = "c_a"

	c.moveSelection(-1)
	if c.selected != "c_a" {
		t.Errorf("selection moved off the top to %q", c.selected)
	}

	c.selected = "c_b"
	c.moveSelection(+1)
	if c.selected != "c_b" {
		t.Errorf("selection moved off the bottom to %q", c.selected)
	}
}

// TestMoveSelectionDefaultsToTheFocusedChild: tabbing into the rail without a
// prior selection must start where you are looking, not at the top.
func TestMoveSelectionDefaultsToTheFocusedChild(t *testing.T) {
	c := newTestCockpit("c_b")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
		summaryFor("c_c", "charlie", 0),
	})
	c.rail.SetFocus("c_b")
	c.selected = ""

	c.moveSelection(+1)

	if c.selected != "c_c" {
		t.Errorf("selection = %q, want c_c (started from focused c_b)", c.selected)
	}
}
