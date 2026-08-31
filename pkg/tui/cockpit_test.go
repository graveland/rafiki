// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	if got := renderRail(nodes, "c_1", "c_1", 24, false); got != "" {
		t.Errorf("renderRail with one child = %q, want empty", got)
	}
}

func TestRailAppearsWithTheSecondChild(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "coordinator", Status: "streaming"},
		{ChildID: "c_2", Name: "scout", ParentID: "c_1", Depth: 1, Status: "idle", Attention: 2},
	}
	got := renderRail(nodes, "c_1", "c_1", 24, false)
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
	lines := strings.Split(strings.TrimRight(renderRail(nodes, "c_1", "c_1", 30, false), "\n"), "\n")
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
	for _, line := range strings.Split(renderRail(nodes, "c_1", "c_1", 20, false), "\n") {
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
	c := newTestCockpit("c_1")
	for _, tc := range []struct {
		key  tea.KeyPressMsg
		name string
		want rafikiv1.SendMode
	}{
		{tea.KeyPressMsg{Code: tea.KeyEnter}, "enter", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, "alt+enter", rafikiv1.SendMode_SEND_MODE_STEER},
		{tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}, "ctrl+s", rafikiv1.SendMode_SEND_MODE_STEER},
		{tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}, "ctrl+x", rafikiv1.SendMode_SEND_MODE_ABORT},
		// esc aborts: it is what muscle memory reaches for to stop a turn.
		{tea.KeyPressMsg{Code: tea.KeyEscape}, "esc", rafikiv1.SendMode_SEND_MODE_ABORT},
		{tea.KeyPressMsg{Code: 'j', Text: "j"}, "j", rafikiv1.SendMode_SEND_MODE_UNSPECIFIED},
	} {
		if got := c.modeForKey(tc.key); got != tc.want {
			t.Errorf("modeForKey(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Inferring the mode from agent state removes a real choice: C1a-2 made a
	// prompt to a busy agent durably QUEUE, so queueing a follow-up and
	// interrupting the running turn are both things a user wants.
	if c.modeForKey(tea.KeyPressMsg{Code: tea.KeyEnter}) ==
		c.modeForKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}) {
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
//
// The focus stream now opens one step later — seed returns the GetHistory
// command and the stream opens in its reply — so the test drives the command
// rather than asserting on the frame after seed. The guarantee is unchanged
// and slightly stronger: nothing is listening on this BaseURL, so the fetch
// FAILS, and the focus stream must still open.
func TestSeedOpensTheFocusStreamForTheInitialChild(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	_, cmd := c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})

	if c.stopRail == nil {
		t.Error("seed must start the rail stream")
	}
	if cmd == nil {
		t.Fatal("seed must return a command for the initially focused child; " +
			"without it the pane only ever sees rail-tier events and stays empty")
	}
	c.Update(cmd())
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
	r.Lines(one, 0, 100)
	r.Lines(one, 0, 100) // settle the cache

	for _, tail := range []string{"BBB-1", "BBB-12", "BBB-123"} {
		two := []session.Block{{Kind: session.KindAssistant, Text: tail}}
		out := strings.Join(r.Lines(two, 0, 100), "\n")
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

// ── input ────────────────────────────────────────────────────────────────────

// The cockpit shipped with a textarea that was never focused. bubbles'
// textarea.Update returns immediately while !m.focus, so every printable key
// was discarded and ⏎ always read an empty value -- the cockpit could not be
// typed into at all, in any pane. No test drove a rune through Update, so
// nothing caught it.
func TestTypingReachesTheTextarea(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, r := range "hello" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := c.ta.Value(); got != "hello" {
		t.Errorf("textarea value = %q, want %q", got, "hello")
	}
}

// The ring OWNS the textarea's focus. Left focused while another pane takes
// keys, it blinks a cursor in an input that is ignoring you; left blurred on
// the way back to input, typing stops working again.
func TestOnlyTheInputPaneHoldsTextareaFocus(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	if !c.ta.Focused() {
		t.Fatal("input pane holds focus at start but the textarea is blurred")
	}
	for _, want := range []struct {
		pane    focusPane
		focused bool
	}{
		{focusRail, false},
		{focusInput, true},
	} {
		c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if c.focus != want.pane {
			t.Fatalf("after ⇥ focus = %v, want %v", c.focus, want.pane)
		}
		if got := c.ta.Focused(); got != want.focused {
			t.Errorf("pane %v: textarea focused = %v, want %v", want.pane, got, want.focused)
		}
	}
}

// Escaping back to input must restore typing, not just the label.
func TestEscapeFromRailRefocusesTheTextarea(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // → rail
	c.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if c.focus != focusInput {
		t.Fatalf("esc left focus at %v, want input", c.focus)
	}
	if !c.ta.Focused() {
		t.Error("esc returned to the input pane with the textarea still blurred")
	}
}

// ^G was a write-only toggle: it flipped showHelp and View never read it, so
// the key documented in the footer and in `rafiki attach --help` did nothing
// at all.
func TestHelpToggleRendersBindings(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := ansi.Strip(c.View().Content)

	c.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	help := ansi.Strip(c.View().Content)
	if help == before {
		t.Fatal("^G changed nothing on screen")
	}
	for _, want := range []string{"steer", "abort", "next pane"} {
		if !strings.Contains(help, want) {
			t.Errorf("help overlay is missing %q:\n%s", want, help)
		}
	}

	c.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if got := ansi.Strip(c.View().Content); got != before {
		t.Error("^G twice did not return to the previous view")
	}
}

// ⇧⏎ is the standard newline in a send-on-⏎ input. It needs an explicit
// binding because BOTH of the textarea's own InsertNewline keys -- enter and
// ^M, which are the same byte -- are taken by Send, so without one a prompt
// can only ever be a single line.
func TestShiftEnterInsertsANewlineAndDoesNotSend(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})

	if got := c.ta.Value(); got != "a\nb" {
		t.Errorf("textarea value = %q, want %q", got, "a\nb")
	}
	if c.pending != "" {
		t.Errorf("⇧⏎ sent %q; it must only insert a newline", c.pending)
	}
}

// ^J is the fallback. A terminal has to speak the Kitty keyboard protocol for
// shift+enter to be reportable at all; without it the key arrives as a bare CR
// and SENDS, which is the one outcome a newline binding must not have.
func TestCtrlJInsertsANewline(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})

	if got := c.ta.Value(); got != "a\nb" {
		t.Errorf("textarea value = %q, want %q", got, "a\nb")
	}
}

// ⏎ still sends, and a multi-line prompt sends whole.
func TestEnterStillSendsTheWholeMultilinePrompt(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if c.pending != "a\nb" {
		t.Errorf("pending = %q, want %q", c.pending, "a\nb")
	}
	if c.ta.Value() != "" {
		t.Errorf("textarea not cleared after send: %q", c.ta.Value())
	}
}

// ── history ──────────────────────────────────────────────────────────────────

func historyEvent(childID, text string, ordinal int32, assistant bool) *rafikiv1.Event {
	blocks := []*rafikiv1.ContentBlock{{
		Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
	}}
	ev := &rafikiv1.Event{ChildId: childID, Ordinal: &ordinal}
	if assistant {
		ev.Payload = &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{Content: blocks}}
	} else {
		ev.Payload = &rafikiv1.Event_UserMessage{
			UserMessage: &rafikiv1.UserMessage{Content: blocks}}
	}
	return ev
}

// The cockpit rendered ONLY the durable event log, which begins whenever the
// event plane was deployed — so every conversation older than the log showed
// as an empty pane, which was all of them. GetHistory already served the whole
// thing in this exact vocabulary and nothing called it.
func TestHistorySeedsTheTranscript(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})

	c.Update(historyMsg{childID: "c_1", after: 4, events: []*rafikiv1.Event{
		historyEvent("c_1", "what is 2+2", 0, false),
		historyEvent("c_1", "four", 1, true),
	}})

	s := c.sessions["c_1"]
	if len(s.Blocks) != 2 {
		t.Fatalf("history produced %d blocks, want 2", len(s.Blocks))
	}
	if s.Blocks[0].Text != "what is 2+2" || s.Blocks[1].Text != "four" {
		t.Errorf("blocks = %q / %q", s.Blocks[0].Text, s.Blocks[1].Text)
	}
}

// THE hazard in wiring history in. GetHistory stamps
// conversation_message.ordinal; the focus stream resumes from
// conversations.event_log.ordinal. They are unrelated sequences. Folding
// history through Apply would leave a 1217-message conversation with a cursor
// of 1216, and the next subscription on a log holding five events would resume
// past its end and receive nothing, forever, with no error anywhere.
func TestHistoryDoesNotMoveTheEventLogCursor(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})

	c.Update(historyMsg{childID: "c_1", after: 4, events: []*rafikiv1.Event{
		historyEvent("c_1", "old prompt", 1216, false),
	}})

	s := c.sessions["c_1"]
	if s.HasCursor {
		t.Fatalf("history set the event-log cursor to %d; the two ordinal spaces are unrelated", s.Cursor)
	}

	// A real log event afterwards must still land.
	c.applyEvent(textEventFor("c_1", "live"))
	if len(s.Blocks) != 2 {
		t.Fatalf("live event after history produced %d blocks, want 2", len(s.Blocks))
	}
}

// A child with no persisted conversation must replay the whole log rather than
// resuming from its head, or a freshly created agent — whose every event is in
// the log and nowhere else — opens on an empty pane.
func TestEmptyHistoryReplaysTheWholeLog(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 7)}})

	c.Update(historyMsg{childID: "c_1", after: 7, events: nil})
	if c.stopFocus == nil {
		t.Fatal("empty history must still open the focus stream")
	}
}

// Hopping back must not re-fetch: the transcript is already in hand and a
// second fetch re-renders the whole conversation on every hop.
func TestHopBackDoesNotRefetchHistory(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 0),
	}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: []*rafikiv1.Event{
		historyEvent("c_1", "old prompt", 0, false),
	}})
	c.applyEvent(textEventFor("c_1", "live")) // gives the session a real cursor

	c.hop("c_2")
	if cmd := c.hop("c_1"); cmd != nil {
		t.Error("hopping back to a child whose transcript is already loaded must not re-fetch history")
	}
}

// ── interaction ──────────────────────────────────────────────────────────────

// A single stray ^C must not throw away an attached session. The key arms, the
// repeat quits — and anything in between disarms, so a ^C now and a ^C a minute
// later are two intentions rather than a quit.
func TestQuitTakesTwoPresses(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: 'd', Mod: tea.ModCtrl},
	} {
		c := newTestCockpit("c_1")
		c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

		if _, cmd := c.Update(k); cmd != nil {
			t.Fatalf("%v: one press quit; it must arm and wait for the repeat", k)
		}
		if c.quitting {
			t.Fatalf("%v: one press set quitting", k)
		}
		if c.notice == "" {
			t.Errorf("%v: an armed quit must say so on screen", k)
		}
		if _, cmd := c.Update(k); cmd == nil {
			t.Fatalf("%v: the repeat must quit", k)
		}
		if !c.quitting {
			t.Errorf("%v: the repeat must set quitting", k)
		}
	}
}

func TestAnotherKeyDisarmsTheQuit(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	c.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if _, cmd := c.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("a keystroke between the two presses must disarm the quit")
	}
	if c.quitting {
		t.Error("a disarmed quit still quit")
	}
}

// A transcript shorter than the pane is bottom-anchored, so the newest line
// sits on the row it will occupy once the conversation is long. A viewport
// renders from the top by default, which made a new conversation start at the
// ceiling and crawl down.
func TestShortTranscriptIsBottomAnchored(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	p := c.pane("c_1")
	c.syncViewport(p, []string{"only line"})

	view := strings.Split(ansi.Strip(p.vp.View()), "\n")
	if len(view) < 2 {
		t.Fatalf("viewport rendered %d lines, want a full pane", len(view))
	}
	if strings.TrimSpace(view[0]) != "" {
		t.Errorf("short transcript starts at the TOP of the pane; want it padded to the bottom:\n%q", view[0])
	}
	last := strings.TrimSpace(view[len(view)-1])
	if last != "only line" {
		t.Errorf("last pane row = %q, want the transcript's newest line", last)
	}
}

// ── scrolling from the input pane ────────────────────────────────────────────

// paneWithContent gives the focused pane more lines than fit, so scroll
// position is observable.
func paneWithContent(t *testing.T, c *Cockpit) *paneState {
	t.Helper()
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i)
	}
	p := c.pane("c_1")
	c.syncViewport(p, lines)
	p.vp.GotoBottom()
	return p
}

// Reaching the transcript must not require leaving the box you type in — the
// reason to read back is usually to decide what to type next.
func TestPageKeysScrollTheTranscriptFromTheInputPane(t *testing.T) {
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if c.focus != focusInput {
		t.Fatal("PgUp moved focus; it must scroll in place")
	}
	if p.vp.YOffset() >= before {
		t.Errorf("PgUp did not scroll: offset %d → %d", before, p.vp.YOffset())
	}
	if c.ta.Value() != "" {
		t.Errorf("PgUp reached the textarea: %q", c.ta.Value())
	}
}

// ↑ is SHARED. With a single-line prompt the cursor has nowhere to go, so the
// key falls through to the transcript.
func TestUpScrollsWhenTheCursorCannotMove(t *testing.T) {
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.vp.YOffset() >= before {
		t.Errorf("↑ on a single-line prompt did not scroll: offset %d → %d", before, p.vp.YOffset())
	}
}

// ...and the textarea keeps it whenever the cursor CAN move, so a multi-line
// prompt is still editable.
func TestUpStaysInTheTextareaWhenItHasSomewhereToGo(t *testing.T) {
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	row := c.ta.Line()
	if row == 0 {
		t.Fatal("fixture is vacuous: the prompt is not multi-line")
	}

	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if c.ta.Line() != row-1 {
		t.Errorf("↑ did not move the textarea cursor: row %d → %d", row, c.ta.Line())
	}
	if p.vp.YOffset() != before {
		t.Error("↑ scrolled the transcript while the cursor still had a line above it")
	}
}

// home/end are top/bottom, FROM THE INPUT PANE. They were transcript-pane-only
// and so appeared not to work at all: with PgUp/PgDn reading from the input
// box nobody tabbed away, and a key that only works in a pane you never visit
// is a key that does not work.
func TestHomeAndEndJumpTheTranscript(t *testing.T) {
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	c.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if p.vp.YOffset() != 0 {
		t.Errorf("home left offset at %d, want the top", p.vp.YOffset())
	}
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if !p.vp.AtBottom() {
		t.Error("end did not reach the bottom")
	}
}

// Naming the focused pane in a grey footer line was not enough: that is not
// where the eye is, so finding it meant cycling ⇥ and watching for a response.
func TestTheFocusedPaneIsMarkedOnScreen(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})

	seen := map[focusPane]string{}
	for _, want := range []focusPane{focusRail, focusInput} {
		c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if c.focus != want {
			t.Fatalf("focus = %v, want %v", c.focus, want)
		}
		raw := c.View().Content
		if !strings.Contains(ansi.Strip(raw), " "+want.String()+" ") {
			t.Errorf("%v: the footer badge does not name the pane", want)
		}
		seen[want] = raw
	}
	// The panes must look DIFFERENT, not merely be named differently: the
	// badge alone is the thing that was already there and was missed.
	if seen[focusRail] == seen[focusInput] {
		t.Error("rail and input focus render identically")
	}
}

// "↓ more below" answered whether you were at the bottom, never where you
// were. The readout is bottom-RIGHT and reports the CONTENT's length: a short
// transcript is padded to bottom-anchor it, and the viewport counts that
// padding as real, so asking it would report 12/12 for a one-line conversation.
func TestScrollPositionReportsWhereYouAre(t *testing.T) {
	c := newTestCockpit("c_1")
	paneWithContent(t, c) // 200 lines, pane is 24 tall

	if got := c.scrollPosition(); !strings.HasSuffix(got, "200/200 100%") {
		t.Errorf("at the bottom the readout = %q, want it to end 200/200 100%%", got)
	}

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	got := c.scrollPosition()
	if !strings.HasPrefix(got, "↓") {
		t.Errorf("scrolled up, readout = %q, want it to mark more below", got)
	}
	if strings.Contains(got, "200/200") {
		t.Errorf("readout did not move after PgUp: %q", got)
	}
	if !strings.Contains(got, "/200") {
		t.Errorf("readout lost the total: %q", got)
	}
}

// A transcript shorter than the pane is padded to sit at the bottom; the
// readout must count the transcript, not the padding.
func TestScrollPositionIgnoresBottomAnchorPadding(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.syncViewport(c.pane("c_1"), []string{"one", "two"})

	if got := c.scrollPosition(); !strings.HasSuffix(got, "2/2 100%") {
		t.Errorf("readout = %q, want 2/2 100%% — the blank padding rows are not transcript", got)
	}
}

// It is right-aligned so it does not move when the key hints do.
func TestScrollPositionIsRightAligned(t *testing.T) {
	c := newTestCockpit("c_1")
	paneWithContent(t, c)
	view := ansi.Strip(c.View().Content)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	last := lines[len(lines)-1]

	if !strings.HasSuffix(strings.TrimRight(last, " "), "100%") {
		t.Errorf("footer does not end with the position readout:\n%q", last)
	}
}

// ⇥ is a TOGGLE, not a three-stop cycle. The transcript pane existed to give
// the viewport its own keymap while a textarea held every plausible scroll key;
// the input pane scrolls directly now, so the third stop bought nothing and
// cost a press on every agent switch — the move made most often.
func TestFocusRingIsATwoStopToggle(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if c.focus != focusRail {
		t.Fatalf("first ⇥ → %v, want agents", c.focus)
	}
	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if c.focus != focusInput {
		t.Fatalf("second ⇥ → %v, want input; the ring must be two stops", c.focus)
	}
}

// With the rail hidden there is nothing to switch to, and ⇥ must leave focus
// where it is rather than parking it on a pane that no longer exists.
func TestTabIsInertWhenTheRailIsHidden(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}) // hide the rail

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if c.focus != focusInput {
		t.Errorf("focus = %v, want input", c.focus)
	}
	if !c.ta.Focused() {
		t.Error("⇥ with the rail hidden left the textarea blurred")
	}
}
