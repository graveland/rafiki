// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
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
	if got := renderRail(nodes, "c_1", 24); got != "" {
		t.Errorf("renderRail with one child = %q, want empty", got)
	}
}

func TestRailAppearsWithTheSecondChild(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "coordinator", Status: "streaming"},
		{ChildID: "c_2", Name: "scout", ParentID: "c_1", Depth: 1, Status: "idle", Attention: 2},
	}
	got := renderRail(nodes, "c_1", 24)
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
	lines := strings.Split(strings.TrimRight(renderRail(nodes, "c_1", 30), "\n"), "\n")
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
	for _, line := range strings.Split(renderRail(nodes, "c_1", 20), "\n") {
		if len([]rune(line)) > 20 {
			t.Errorf("row is %d runes, want <= 20: %q", len([]rune(line)), line)
		}
	}
}

func TestClipCountsRunesNotBytes(t *testing.T) {
	// A child name holds whatever a spawner typed. Byte truncation would split
	// a rune and corrupt the line.
	got := clip("日本語のエージェント", 5)
	if len([]rune(got)) != 5 {
		t.Errorf("clip = %q (%d runes), want 5 runes", got, len([]rune(got)))
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
