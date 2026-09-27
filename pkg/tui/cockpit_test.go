// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
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

// turnEndWithUsageFor is a turn_end carrying token usage, for driving
// contextReadout through the rail's Apply path.
func turnEndWithUsageFor(id string, ord int32, input, cacheRead, cacheWrite int64) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{Usage: &rafikiv1.Usage{
			InputTokens:      &input,
			CacheReadTokens:  &cacheRead,
			CacheWriteTokens: &cacheWrite,
		}}}}
}

func textEventFor(id, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}}}
}

// userMessageEventFor is textEventFor carrying a durable ordinal, the shape
// the focus stream actually delivers.
func userMessageEventFor(id, text string, ord int32) *rafikiv1.Event {
	ev := textEventFor(id, text)
	ev.Ordinal = &ord
	return ev
}

func statusEventFor(id, state string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}}}
}

// ── rail rendering ───────────────────────────────────────────────────────────

// renderRail renders whatever it is given, one row included: the "stay
// hidden below two rows" DEFAULT look moved to railCols, because a renderer
// that suppresses on its own cannot be overridden — and an explicit ^R has
// to be able to reveal the one-row rail (the peek in railCols is what makes
// that render). railCols()==0 with one agent and no peek is pinned by
// TestCtrlRPeeksTheOneRowRail below.
func TestRailRendersASingleChildRow(t *testing.T) {
	c := assert.NewCollecting(t)
	nodes := []rail.Node{{ChildID: "c_1", Name: "coordinator", Status: "idle"}}
	got := renderRail(nodes, "c_1", "c_1", 24, false, nil, 0)
	c.Require().NotEq("", got, "renderRail with one child must render the row it was given")
	c.StrContains(got, "coordinator", "rail missing the only row:\n")
}

func TestRailAppearsWithTheSecondChild(t *testing.T) {
	c := assert.NewCollecting(t)
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "coordinator", Status: "streaming"},
		{ChildID: "c_2", Name: "scout", ParentID: "c_1", Depth: 1, Status: "idle", Attention: 2},
	}
	got := renderRail(nodes, "c_1", "c_1", 24, false, nil, 0)
	c.Require().NotEq("", got, "renderRail with two children must render")
	for _, want := range []string{"coordinator", "scout", "2", rail.AnimatedGlyph(nodes[0], 0)} {
		c.StrContains(got, want, "rail missing")
	}
}

// ── status line identity & working spinner ──────────────────────────────────

func TestStatusLineShowsFocusedAgentIdentity(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Cwd: "/work/scout", Labels: map[string]string{}},
	})

	got := ansi.Strip(c.View().Content)
	ck.StrContains(got, "scout", "status line missing the agent name:\n")
	ck.StrContains(got, "/work/scout", "status line missing the agent path:\n")
}

// A bare `rafiki attach` has nothing focused (NewCockpit starts the rail
// focused instead), so there is no identity to show and no " · " separator
// should appear at all.
func TestStatusLineIdentityAbsentWithoutAFocusedChild(t *testing.T) {
	c := newTestCockpit("")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	got := ansi.Strip(c.View().Content)
	assert.NewCollecting(t).NotStrContains(got, " · ", "no agent is focused, but the identity separator showed up:\n")
}

// The footer names the active profile only when the caller says to -- this
// package must not decide that for itself (see Options.ShowProfileBadge).
func TestFooterShowsProfileBadgeWhenToldTo(t *testing.T) {
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", ProfileName: "work", ShowProfileBadge: true})
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	got := ansi.Strip(c.View().Content)
	assert.NewCollecting(t).StrContains(got, "work", "footer missing the profile badge:\n")
}

// A single profile is unambiguous, so ShowProfileBadge is false and the
// footer must not mention the profile at all -- a noise badge for someone
// who never switches costs something for nothing.
func TestFooterOmitsProfileBadgeWhenNotToldTo(t *testing.T) {
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", ProfileName: "work", ShowProfileBadge: false})
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	got := ansi.Strip(c.View().Content)
	assert.NewCollecting(t).NotStrContains(got, "work", "profile badge shown despite ShowProfileBadge=false:\n")
}

// The footer shows how close a capped agent is to its budget, not just what
// it has spent -- see costReadout's doc comment.
func TestCostReadoutShowsCapAlongsideSpend(t *testing.T) {
	c := newTestCockpit("c_1")
	maxCost := 5.0
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &maxCost},
	})
	c.rail.SetCost("c_1", 1.23)

	got := c.costReadout()
	assert.NewCollecting(t).False(!strings.Contains(got, "1.23") || !strings.Contains(got, "5.00"), "costReadout() = %q, want spend and cap both present", got)
}

// No spend yet means costReadout stays silent even with a cap set -- a wall
// of $0.00/$5.00 beside every freshly spawned capped agent is noise, matching
// the existing zero-suppression rule for spend with no cap at all.
func TestCostReadoutOmitsCapWhenNothingSpentYet(t *testing.T) {
	c := newTestCockpit("c_1")
	maxCost := 5.0
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &maxCost},
	})

	assert.NewCollecting(t).Eq("", c.costReadout(), "costReadout()")
}

// An uncapped agent's readout is unchanged: just the spend, no suffix.
func TestCostReadoutOmitsCapWhenUnset(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "uncapped", Status: "idle", Labels: map[string]string{}},
	})
	c.rail.SetCost("c_1", 1.23)

	got := c.costReadout()
	assert.NewCollecting(t).False(!strings.Contains(got, "1.23") || strings.Contains(got, "/"), "costReadout() = %q, want spend with no cap suffix", got)
}

// Nothing has completed a turn yet, so there is no prompt size to show.
func TestContextReadoutEmptyBeforeAnyTurn(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "scout", 0)})

	assert.NewCollecting(t).Eq("", c.contextReadout(), "contextReadout()")
}

// With a known context window, the readout shows current/max and a percent.
func TestContextReadoutShowsWindowAndPercent(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", ContextWindow: 200_000},
	})
	c.rail.Apply(turnEndWithUsageFor("c_1", 1, 50_000, 12_000, 0)) // 62000 total

	got := c.contextReadout()
	assert.NewCollecting(t).Eq("ctx:62k/200k (31%)", got, "contextReadout() = %q, want ctx:62k/200k (31%%)", got)
}

// No catalog entry means no known window -- every locally-served model has
// none -- so the readout shows the token count alone rather than guessing a
// percentage against an unknown denominator.
func TestContextReadoutOmitsPercentWhenWindowUnknown(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "scout", 0)}) // ContextWindow 0
	c.rail.Apply(turnEndWithUsageFor("c_1", 1, 10_000, 0, 0))

	got := c.contextReadout()
	ck.Eq("ctx:10k", got, "contextReadout()")
	ck.False(strings.Contains(got, "%") || strings.Contains(got, "/"), "contextReadout() = %q, must not guess a percent or a max", got)
}

// CtxTokens == 0 is the same "nothing to show yet" case as before any turn --
// guarded explicitly so a completed turn reporting zero usage never renders
// "ctx:0".
func TestContextReadoutEmptyWhenTokensAreZero(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", ContextWindow: 200_000},
	})
	c.rail.Apply(turnEndWithUsageFor("c_1", 1, 0, 0, 0))

	assert.NewCollecting(t).Eq("", c.contextReadout(), "contextReadout()")
}

// Rounding: 1000/128000 rounds to 1%, not truncates to 0.
func TestContextReadoutRoundsThePercent(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", ContextWindow: 128_000},
	})
	c.rail.Apply(turnEndWithUsageFor("c_1", 1, 645, 0, 0)) // 645/128000 = 0.504% -> rounds to 1%

	got := c.contextReadout()
	assert.NewCollecting(t).True(strings.HasSuffix(got, "(1%)"), "contextReadout() = %q, want a rounded 1%%", got)
}

// The identity must clip rather than overflow -- a long cwd on a narrow
// terminal must not push the status line past the window width.
func TestStatusLineIdentityClipsToWidth(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle",
			Cwd: "/very/long/path/that/will/not/fit/on/a/narrow/terminal", Labels: map[string]string{}},
	})

	// Only the status line itself is under test here -- the footer's key
	// hints are a separate, pre-existing overflow this change does not touch.
	got := ansi.Strip(c.View().Content)
	found := false
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "scout") {
			continue
		}
		found = true
		w := ansi.StringWidth(line)
		ck.LessOrEqual(40, w, "status line is %d columns wide, budget is 40: %q", w, line)
	}
	ck.Require().True(found, "no line contained the agent name at all -- identity was dropped, not clipped")
}

func TestTranscriptShowsWorkingSpinnerWhenBusy(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "streaming", Labels: map[string]string{}},
	})

	got := ansi.Strip(c.View().Content)
	assert.NewCollecting(t).StrContains(got, "streaming…", "transcript missing the working spinner line:\n")
}

func TestTranscriptHasNoWorkingSpinnerWhenIdle(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Labels: map[string]string{}},
	})

	got := ansi.Strip(c.View().Content)
	for _, label := range []string{"streaming…", "running tool…", "compacting…", "working…"} {
		assert.NewCollecting(t).NotStrContains(got, label, "idle transcript should not show a working spinner, found")
	}
}

// A pending send takes priority over the working spinner so the two do not
// flicker between each other in the instant before the first status event
// confirms the turn started.
func TestPendingSuppressesTheWorkingSpinner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "streaming", Labels: map[string]string{}},
	})
	c.pending = "hello"

	got := ansi.Strip(c.View().Content)
	ck.NotStrContains(got, "streaming…", "pending send did not suppress the working spinner")
	ck.StrContains(got, "⏳ hello", "pending line missing:\n")
}

func TestRailIndentsByDepth(t *testing.T) {
	c := assert.NewCollecting(t)
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "root", Status: "idle"},
		{ChildID: "c_2", Name: "kid", ParentID: "c_1", Depth: 1, Status: "idle"},
		{ChildID: "c_3", Name: "grandkid", ParentID: "c_2", Depth: 2, Status: "idle"},
	}
	lines := strings.Split(strings.TrimRight(renderRail(nodes, "c_1", "c_1", 30, false, nil, 0), "\n"), "\n")
	c.Require().GreaterOrEqual(3, len(lines), "want 3 rows, got %d: %v", len(lines), lines)
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	for i := 1; i < 3; i++ {
		c.Greater(indent(lines[i-1]), indent(lines[i]), "row %d must indent deeper than row %d:\n%v", i, i-1, lines)
	}
}

func TestRailRowsAreClippedToWidth(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "a", Status: "idle"},
		{ChildID: "c_2", Name: strings.Repeat("verylongname", 20), Status: "idle"},
	}
	for _, line := range strings.Split(renderRail(nodes, "c_1", "c_1", 20, false, nil, 0), "\n") {
		assert.NewCollecting(t).LessOrEqual(20, len([]rune(line)), "row is %d runes, want <= 20: %q", len([]rune(line)), line)
	}
}

func TestClipCountsRunesNotBytes(t *testing.T) {
	c := assert.NewCollecting(t)
	// A child name holds whatever a spawner typed. Byte truncation would split
	// a rune and corrupt the line. clip measures DISPLAY COLUMNS (a CJK glyph
	// is one rune but two columns) — see TestClipMeasuresDisplayWidthNotRunes
	// in railview_test.go for the fuller regression pinning this.
	got := clip("日本語のエージェント", 5)
	w := ansi.StringWidth(got)
	c.Eq(5, w, "clip = %q (%d display columns), want 5", got, w)
	c.True(strings.HasSuffix(got, "…"), "clip = %q, want an ellipsis", got)
}

// ── hop, LRU, keys ───────────────────────────────────────────────────────────

func TestHopRetainsTheOldTranscript(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})
	c.sessions["c_1"].Apply(textEventFor("c_1", "keep me"))

	c.hop("c_2")
	defer c.shutdown()

	ck.Require().NotNil(c.sessions["c_1"], "hopping away must KEEP the transcript: a full replay on every hop back is "+
		"the cost the cockpit exists to remove")
	ck.Len(c.sessions["c_1"].Blocks, 1, "blocks = %d, want 1", len(c.sessions["c_1"].Blocks))
	ck.Eq("c_2", c.focused(), "focused")
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
	assert.NewCollecting(t).False(n.Attention != 0 || n.Seen != 5, "c_1 = attention %d seen %d, want 0/5", n.Attention, n.Seen)
}

// c.status is a single global field: nothing else resets it on a focus
// change, so without this the footer keeps showing whatever the previously
// focused child last reported -- including a status belonging to a different
// conversation entirely.
func TestHopUpdatesTheStatusLineToTheNewChild(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})
	c.rail.Apply(statusEventFor("c_2", "shutting_down", 1))
	c.status = "agent: idle" // stale, left over from c_1

	c.hop("c_2")

	assert.NewCollecting(t).Eq("agent: shutting_down", c.status, "status")
}

// MarkRead is otherwise only called reactively, when a NEW event arrives for
// the focused child. Hopping into a child that was already loaded and simply
// re-reading its (already-fetched) transcript must clear its badge too,
// rather than leaving it until the next live event -- which may never come
// for a quiet conversation.
func TestHopClearsAnExistingUnreadBadgeOnAnAlreadyLoadedChild(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})

	// c_2 was visited before (it has a cursor) and earned a badge while c_1
	// was focused.
	c.sessions["c_2"] = session.New("c_2")
	ev := statusEventFor("c_2", "idle", 3)
	c.sessions["c_2"].Apply(ev)
	c.rail.Apply(ev)

	if n, _ := c.rail.Get("c_2"); n.Attention == 0 {
		t.Fatal("test setup: c_2 should carry a badge before the hop")
	}

	c.hop("c_2")

	if n, _ := c.rail.Get("c_2"); n.Attention != 0 {
		t.Errorf("Attention = %d, want 0: focusing and reading an already-loaded child must clear its badge immediately", n.Attention)
	}
}

func TestLRUEvictsTheOldestButNeverTheFocused(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_0")
	defer c.shutdown()
	for i := 1; i <= maxSessions+2; i++ {
		c.hop("c_" + strconv.Itoa(i))
	}
	ck.Require().LessOrEqual(maxSessions, len(c.sessions), "sessions")
	if _, ok := c.sessions["c_0"]; ok {
		t.Error("the least-recently-focused session should have been evicted")
	}
	_, ok := c.sessions[c.focused()]
	ck.True(ok, "the FOCUSED session must never be evicted")
}

func TestSendModesAreDistinctKeys(t *testing.T) {
	ck := assert.NewCollecting(t)
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
		got := c.modeForKey(tc.key)
		ck.Eq(tc.want, got, "modeForKey(%q) = %v, want", tc.name, got)
	}
	// Inferring the mode from agent state removes a real choice: C1a-2 made a
	// prompt to a busy agent durably QUEUE, so queueing a follow-up and
	// interrupting the running turn are both things a user wants.
	ck.Require().NotEq(c.modeForKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}), c.modeForKey(tea.KeyPressMsg{Code: tea.KeyEnter}), "prompt and steer must not collapse onto one key")
}

// child_spawned is the only event that introduces a rail row, and a child
// spawned during a disconnect has its child_spawned in the past -- the server
// replays only children named in the cursor. Without the self-heal that child
// is invisible for the rest of the session.
func TestTrafficFromAnUnknownChildTriggersAReseed(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})

	c.applyEvent(turnEndFor("c_ghost", 3))
	ck.Require().True(c.reseeding, "an event from an unknown child must request a re-seed")

	c.reseeding = false
	c.applyEvent(turnEndFor("c_1", 4))
	ck.False(c.reseeding, "a known child must not trigger a re-seed")
}

func TestNeighbourWrapsInDisplayOrder(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.rail.SetFocus("c_a")
	ck.Eq("c_b", c.neighbour(-1), "neighbour(-1) from the first row")
	ck.Eq("c_b", c.neighbour(+1), "neighbour(+1)")
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
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	_, cmd := c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})

	ck.NotNil(c.stopRail, "seed must start the rail stream")
	ck.Require().NotNil(cmd, "seed must return a command for the initially focused child; "+
		"without it the pane only ever sees rail-tier events and stays empty")
	c.Update(cmd())
	ck.Require().NotNil(c.stopFocus, "seed must open a FOCUS stream for the initially focused child; "+
		"without it the pane only ever sees rail-tier events and stays empty")
}

// FINDING 2. The rail subscription covers every child in the subject but
// carries none of their content. Applying it to a retained session nothing is
// streaming pushed that session's cursor past what it had rendered, and the
// next open resumes from exactly that cursor -- so everything the agent
// produced while you were away was skipped, silently and permanently.
func TestRailEventsDoNotAdvanceANonFocusedSessionsCursor(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0)})

	// The subscription is on c_1, which reaches ordinal 10 through it. (The
	// test drives applyEvent directly, so it names the owner the way an open
	// stream would have.)
	c.focusChild = "c_1"
	c.applyEvent(&rafikiv1.Event{ChildId: "c_1", Ordinal: ptr32(10),
		Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{}}})
	ck.Eq(10, c.sessions["c_1"].Cursor, "focused cursor")

	// Hop away. The subscription MOVES only when the target's history lands --
	// which is the moment c_1's stream actually stops -- so deliver it, then
	// send a focus-tier event for c_1: nobody is streaming c_1 any more, so it
	// must not advance its session.
	c.hop("c_2")
	c.Update(historyMsg{childID: "c_2", after: 0, events: nil})
	c.applyEvent(turnEndFor("c_1", 250))

	got := c.sessions["c_1"].Cursor
	ck.Eq(10, got, "non-focused cursor = %d, want 10 -- a delivered event advanced it, so hopping "+
		"back would resume from %d and skip ordinals 11..%d forever", got, got, got)
}

// FINDING 3. One renderer is shared by every session and its live-tail cache is
// keyed on a fingerprint, not on a child. The store used to be guarded by
// `if !needRender`, so a CHANGED tail computed a fresh string and then emitted
// the stale one -- the previous child's half-finished paragraph, for the whole
// of the next child's turn.
func TestRendererDoesNotBleedAcrossSessions(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newRenderer()
	one := []session.Block{{Kind: session.KindAssistant, Text: "AAA-from-child-one"}}
	r.Lines(one, 0, 100)
	r.Lines(one, 0, 100) // settle the cache

	for _, tail := range []string{"BBB-1", "BBB-12", "BBB-123"} {
		two := []session.Block{{Kind: session.KindAssistant, Text: tail}}
		out := strings.Join(r.Lines(two, 0, 100), "\n")
		c.Require().NotStrContains(out, "AAA-from-child-one", "render of %q leaked the previous child's tail:\n", tail)
		c.StrContains(out, tail, "render of")
	}
}

// waitForEvent must collapse a burst already sitting on the channel into one
// eventMsg -- this is what stops a large conversation's history-fallback
// replay from visibly scrolling past one event (one Update/View cycle) at a
// time. It must not, however, wait around for MORE than what is already
// queued: an isolated live event arriving alone must return immediately with
// exactly itself, not block hoping for company.
func TestWaitForEventDrainsWhatIsAlreadyQueued(t *testing.T) {
	c := assert.NewCollecting(t)
	ch := make(chan *rafikiv1.Event, 8)
	for i := int32(0); i < 5; i++ {
		ch <- turnEndFor("c_1", i)
	}

	msg := waitForEvent(ch)()
	em, ok := msg.(eventMsg)
	c.Require().True(ok, "waitForEvent() = %T, want eventMsg", msg)
	c.Require().Len(em.evs, 5, "drained %d events, want all 5 already queued", len(em.evs))
	for i, ev := range em.evs {
		c.Eq(int32(i), ev.GetOrdinal(), "evs[%d] ordinal = %d, want %d -- order must survive the drain", i, ev.GetOrdinal(), i)
	}
}

func TestWaitForEventReturnsASingleIsolatedEventImmediately(t *testing.T) {
	c := assert.NewAborting(t)
	ch := make(chan *rafikiv1.Event, 8)
	ch <- turnEndFor("c_1", 0)

	msg := waitForEvent(ch)()
	em, ok := msg.(eventMsg)
	c.True(ok, "waitForEvent() = %T, want eventMsg", msg)
	c.Len(em.evs, 1, "drained %d events, want exactly the 1 queued", len(em.evs))
}

// FINDING 4. reseeding was set by applyEvent and cleared only when the RPC
// returned, and the eventMsg case dispatched on it every time -- so each event
// arriving during a slow ListChildren queued another concurrent one. The
// cockpit amplified against a daemon that was already slow, which is the exact
// condition the self-heal exists for.
func TestReseedDispatchesAtMostOneInFlight(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})

	dispatched := 0
	for i := int32(0); i < 5; i++ {
		_, cmd := c.Update(eventMsg{evs: []*rafikiv1.Event{turnEndFor("c_ghost", i)}})
		if cmd != nil {
			// tea.Batch always returns non-nil; count the re-seed explicitly.
			if c.reseedInFlight && !c.reseeding {
				dispatched++
				c.reseeding = false
			}
		}
	}
	ck.Require().Eq(1, dispatched, "dispatched")

	// The reply releases the latch so a later gap can still self-heal.
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})
	ck.False(c.reseedInFlight, "seedMsg must clear reseedInFlight")
}

// FINDING 5. ListChildrenRequest has no subject filter, so an unfiltered seed
// installed a row for every live child on the daemon. Rows outside the subject
// are frozen by construction -- their events never match -- so they keep their
// seed-time glyph forever, never badge, and still absorb focus.
func TestSeedIsNarrowedToTheSubject(t *testing.T) {
	c := assert.NewCollecting(t)
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
		c.False(!got[want], "%s missing from the subtree seed; got %v", want, got)
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
	c.Eq(len(kids), all.rail.Len(), "subject `all` seeded")
}

func ptr32(v int32) *int32 { return &v }

var errSeedDown = errors.New("daemon unreachable")

// A failed seed used to end the flow: the cockpit waited for the next
// unrelated event to re-attempt ListChildren, and against the slow daemon the
// self-heal exists for that event may be a long time coming -- an initial
// seed that failed left a cockpit with no rail stream at all. The retry now
// schedules itself on the stream layer's capped backoff and the attempt count
// resets on the first success.
func TestFailedSeedRetriesOnTheBackoffSchedule(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()

	// First failure: the attempt count climbs and a retry is scheduled --
	// nothing else would re-attempt ListChildren on a quiet daemon.
	_, cmd := c.Update(seedMsg{err: errSeedDown})
	ck.False(c.reseedInFlight, "a failed seed must release reseedInFlight")
	ck.Eq(1, c.seedAttempt, "seedAttempt")
	ck.NotNil(cmd, "a failed seed must schedule a retry, not wait for the next event")

	// The timer fires and re-dispatches ListChildren. Nothing is listening on
	// this BaseURL, so driving the seed command to completion fails fast.
	msg := cmd()
	if _, ok := msg.(seedRetryMsg); !ok {
		t.Fatalf("retry command produced %T, want seedRetryMsg", msg)
	}
	_, seedCmd := c.Update(seedRetryMsg{})
	ck.False(!c.reseedInFlight || seedCmd == nil, "seedRetryMsg with no seed running must dispatch ListChildren")

	// That attempt fails too; the count climbs and another retry is scheduled.
	if sm, ok := seedCmd().(seedMsg); !ok || sm.err == nil {
		t.Fatalf("seed command produced %T, want a failing seedMsg", seedCmd())
	}
	_, cmd2 := c.Update(seedMsg{err: errSeedDown})
	if c.seedAttempt != 2 || cmd2 == nil {
		t.Fatalf("after a second failure seedAttempt = %d, cmd = %v; want 2 and a retry", c.seedAttempt, cmd2)
	}

	// A retry timer arriving while a seed IS in flight must not queue a second
	// one -- its completion re-checks reseeding instead.
	c.reseeding, c.reseedInFlight = false, true
	if _, cmd := c.Update(seedRetryMsg{}); cmd != nil || !c.reseedInFlight {
		t.Fatal("seedRetryMsg during an in-flight seed must defer to it")
	}

	// Success resets the schedule to its fast end.
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})
	ck.Eq(0, c.seedAttempt, "seedAttempt")
}

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
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0),
		summaryFor("c_b", "bravo", 0),
		summaryFor("c_c", "charlie", 0),
	})
	c.rail.SetFocus("c_a")
	c.selected = "c_a"

	c.moveSelection(+1)

	ck.Eq("c_b", c.selected, "selection")
	got := c.rail.Focus()
	ck.Eq("c_a", got, "moving the selection changed focus to %q; focus must only "+
		"change on commit", got)
}

// TestMoveSelectionClampsAtTheEnds: selection clamps where neighbour() wraps.
// Two bindings that both wrap are indistinguishable in use, and wrapping is
// what the attention jump does.
func TestMoveSelectionClampsAtTheEnds(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.selected = "c_a"

	c.moveSelection(-1)
	ck.Eq("c_a", c.selected, "selection moved off the top to")

	c.selected = "c_b"
	c.moveSelection(+1)
	ck.Eq("c_b", c.selected, "selection moved off the bottom to")
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

	assert.NewCollecting(t).Eq("c_c", c.selected, "selection")
}

// ── rail preview ───────────────────────────────────────────────────────────

// The point of browsing without committing: ↑/↓ over the rail pages through the
// highlighted agents' transcripts while focus -- and the input, and the live
// feed -- stays where it was. ⏎ is what makes the highlighted agent the live,
// typed-to one.
func TestArrowsPreviewTheHighlightedAgentWithoutHopping(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.rail.SetFocus("c_a")
	c.Update(keyMsg("tab")) // → agents pane; the cursor starts on the focused child
	c.sessions["c_b"] = session.New("c_b")
	c.sessions["c_b"].Apply(textEventFor("c_b", "bravo says hi"))

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})

	ck.Eq("c_a", c.rail.Focus(), "an arrow moved focus to")
	content := ansi.Strip(c.View().Content)
	ck.StrContains(content, "bravo says hi", "arrowing down did not preview c_b's transcript:\n")
}

// ⏎ is where reading ends and talking begins: the highlighted agent becomes
// the focused one and the keys go back to the input.
func TestCommitSwitchesFocusAndEntersTheInput(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.rail.SetFocus("c_a")
	c.Update(keyMsg("tab"))
	c.Update(tea.KeyPressMsg{Code: tea.KeyDown})

	c.Update(keyMsg("enter"))

	defer c.shutdown()
	ck.Eq("c_b", c.rail.Focus(), "⏎ left focus on")
	ck.Eq(focusInput, c.focus, "⏎ left focus on the agents pane; it must hand the keys to the input")
	ck.True(c.ta.Focused(), "⏎ returned to the input pane with the textarea still blurred")
	// The commit is also where the browse overlay comes down: the cockpit is
	// either browsing (rail focused) or talking (full width), never a rail
	// drawn beside an unfocused input. Pinned in full by
	// TestEveryRailExitHidesTheRail.
	ck.Eq(0, c.railCols(), "⏎ left the rail drawn; committing must land full width")
}

// The cockpit holds two modes and nothing between them: BROWSE (rail on screen
// and focused) and TALK (full-width transcript, keys in the input). Any exit
// from the rail takes the overlay down -- ⏎ because talking to the chosen
// agent is the point of the browse, esc because "done looking" ends it too,
// ⇥ because cycling out of the ring is leaving it. A rail left drawn beside an
// unfocused input is the state that made focus ambiguous.
func TestEveryRailExitHidesTheRail(t *testing.T) {
	for _, key := range []string{"enter", "esc", "tab"} {
		t.Run(key, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c := railWith(t, "c_1", "c_2")
			defer c.shutdown()

			if c.focus != focusRail || c.railCols() == 0 {
				t.Fatalf("setup: focus = %v, railCols = %d; the rail never took focus", c.focus, c.railCols())
			}

			c.Update(keyMsg(key))

			ck.Eq(0, c.railCols(), "%s from the rail left it drawn", key)
			ck.Eq(focusInput, c.focus, "%s left focus = %v, want input", key, c.focus)
			ck.True(c.ta.Focused(), "%s left the textarea blurred", key)
		})
	}
}

// ^R is the one way to hold the rail up unfocused -- the watch mode, badges
// and costs visible while the keys stay in the input -- and even that pin ends
// at the next visit: every exit from the rail hides it, so the pin survives
// only until the rail is entered and left again. The mode rule has exactly one
// exception and this pins its boundary.
func TestRailPinEndsAtTheNextVisit(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	defer c.shutdown()
	ctrlR(c) // hide
	ctrlR(c) // show again, unfocused: the watch-mode pin

	ck.Require().NotEq(0, c.railCols(), "the second ^R did not reveal the rail")
	ck.Require().Eq(focusInput, c.focus, "focus")

	c.Update(keyMsg("tab")) // visit the rail
	c.Update(keyMsg("esc")) // and leave it

	ck.Eq(0, c.railCols(), "the pin survived a visit to the rail")
	ck.Eq(focusInput, c.focus, "focus")
}

// Browsing pages the pane through the highlighted agents' transcripts LIVE:
// once the cursor rests, the single focus subscription moves to the viewed
// child -- the same slot hop uses -- so a working agent's pane tracks it at
// delta granularity instead of re-reading the whole conversation per event.
// The rest gate that keeps this off one-stream-per-keystroke is pinned by
// TestSweepDebouncesThePreviewFetch.
func TestPreviewMovesTheSubscriptionToTheViewedChild(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 9),
	}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil}) // opens c_1's stream
	assert.NewAborting(t).False(c.focusChild != "c_1" || c.stopFocus == nil, "setup: c_1's focus stream never opened")

	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq}) // the cursor rested on c_2
	c.Update(historyMsg{childID: "c_2", after: 9, events: []*rafikiv1.Event{
		historyEvent("c_2", "preview line", 0, false),
	}})

	if c.focusChild != "c_2" || c.stopFocus == nil {
		t.Errorf("resting on c_2 left the subscription on %q; the preview is live, not a snapshot", c.focusChild)
	}
	if s := c.sessions["c_2"]; s == nil || len(s.Blocks) != 1 {
		t.Fatalf("the previewed transcript did not load; blocks = %d", len(s.Blocks))
	}
	// The moved subscription feeds the VIEWED session -- the ordinal is a log
	// ordinal past the fetch's watermark, as the live stream would deliver it.
	c.Update(eventMsg{evs: []*rafikiv1.Event{userMessageEventFor("c_2", "live line", 12)}})
	if s := c.sessions["c_2"]; len(s.Blocks) != 2 {
		t.Errorf("the live preview did not feed the viewed session; blocks = %d", len(s.Blocks))
	}
	if n, ok := c.rail.Get("c_2"); !ok || n.Attention != 0 {
		t.Errorf("watching a live preview left attention at %d; delivery to the displayed session is reading", n.Attention)
	}
}

// Leaving the rail -- any exit, not just ⏎ -- returns the subscription to the
// committed agent: the pane about to be shown is its, and its stream is the
// one that must be running. The browsed child's stream stops; on the next
// visit it resumes from the cursor it kept, which is what makes the gap safe.
func TestLeaveRailReturnsTheSubscriptionToTheCommittedAgent(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 9),
	}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil})
	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq})
	c.Update(historyMsg{childID: "c_2", after: 9, events: []*rafikiv1.Event{
		historyEvent("c_2", "preview line", 0, false),
	}})
	ck.Require().Eq("c_2", c.focusChild, "setup: browsing did not move the subscription (owner")

	c.Update(keyMsg("esc"))

	ck.Require().Eq(focusInput, c.focus, "esc did not return to the input")
	ck.Eq("c_1", c.focusChild, "after esc the subscription sits on")
}

// The browsed child's session keeps the cursor its live feed advanced, so a
// hop back resumes from THERE and replays nothing twice. This is the record
// of why the subscription moves at all instead of the pane reading snapshots:
// the state stays honest for the return trip.
func TestHopBackToABrowsedChildResumesFromItsLiveCursor(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 9),
	}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil})
	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq})
	c.Update(historyMsg{childID: "c_2", after: 9, events: []*rafikiv1.Event{
		historyEvent("c_2", "preview line", 0, false),
	}})
	// Live delivery advances the browsed session past the watermark.
	c.Update(eventMsg{evs: []*rafikiv1.Event{userMessageEventFor("c_2", "live line", 12)}})
	if s := c.sessions["c_2"]; s.Cursor != 12 {
		t.Fatalf("live delivery left c_2's cursor at %d, want 12", s.Cursor)
	}

	c.Update(keyMsg("esc")) // subscription returns to c_1
	c.hop("c_2")

	assert.NewAborting(t).Eq("c_2", c.focusChild, "hop to the browsed child left the subscription on")
	// Resuming re-opens from the session cursor (12); the already-applied
	// ordinals are not replayed into duplicated blocks.
	if s := c.sessions["c_2"]; len(s.Blocks) != 2 {
		t.Errorf("blocks = %d, want 2; the resume must not duplicate applied content", len(s.Blocks))
	}
}

// The watermark discipline survives the live preview: history delivery stamps
// the session with the log ordinal captured BEFORE the fetch and the stream
// opens resuming from exactly that -- so nothing logged while the fetch ran is
// skipped, and the next open resumes from where the pane actually is.
func TestPreviewDeliveryResumesFromTheWatermark(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 9),
	}})

	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq})
	c.Update(historyMsg{childID: "c_2", after: 9, events: []*rafikiv1.Event{
		historyEvent("c_2", "old prompt", 0, false),
	}})
	s := c.sessions["c_2"]
	if !s.HasCursor || s.Cursor != 9 {
		t.Fatalf("preview left cursor at %d/%v, want the watermark 9; the next open would resume from the wrong place", s.Cursor, s.HasCursor)
	}
	ck.Eq("c_2", c.focusChild, "delivering the preview did not open its subscription (owner")

	// And committing to the row the subscription is already on must not churn
	// it: openFocus's owner check makes ⏎ on the previewed row free.
	c.hop("c_2")
	ck.Eq("c_2", c.focusChild, "committing to the previewed child moved the subscription to")
}

// Displayed is read -- but ONLY what was displayed. A preview whose fetch
// landed while the cursor was still on the row clears the badge; one that
// landed after the cursor moved on does not, or browsing past an agent would
// silently mark messages nobody read.
func TestPreviewMarksReadOnlyWhenDisplayed(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 5),
	}})
	c.Update(railEventMsg{evs: []*rafikiv1.Event{turnEndFor("c_2", 8)}})
	if n, ok := c.rail.Get("c_2"); !ok || n.Attention != 1 {
		t.Fatalf("setup: c_2 carries attention %v", n.Attention)
	}

	// Browse onto c_2 and let its preview land while it is still displayed.
	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq})
	c.Update(historyMsg{childID: "c_2", after: 8, events: []*rafikiv1.Event{
		historyEvent("c_2", "hi", 0, false),
	}})
	if n, ok := c.rail.Get("c_2"); !ok || n.Attention != 0 {
		t.Errorf("displaying the preview left attention at %d; showing it must read it", n.Attention)
	}

	// And browse away before the fetch lands: nothing was displayed, so the
	// badge must survive, the late delivery must warm the session without
	// opening anything, and the subscription must stay exactly where it was.
	c.moveSelection(-1) // back to c_1
	before := c.focusChild
	c.Update(railEventMsg{evs: []*rafikiv1.Event{turnEndFor("c_2", 9)}})
	c.Update(historyMsg{childID: "c_2", after: 8, events: []*rafikiv1.Event{
		historyEvent("c_2", "more", 1, true),
	}})
	if n, ok := c.rail.Get("c_2"); !ok || n.Attention != 1 {
		t.Errorf("an undisplayed preview cleared the badge: attention = %d", n.Attention)
	}
	if s := c.sessions["c_2"]; len(s.Blocks) != 2 {
		t.Errorf("the late fetch should still warm the session; blocks = %d", len(s.Blocks))
	}
	assert.NewCollecting(t).Eq(before, c.focusChild, "a late delivery moved the subscription to")
}

// GetHistory is the most expensive RPC this client issues and a stream move is
// a subscription churn, so a held arrow key over a fleet must fire neither per
// row it passes. The arrow only arms a debounce, and the tick armed under an
// older cursor position is a no-op -- a sweep costs one action at the final
// rest, and never two fetches for the same child at once.
func TestSweepDebouncesThePreviewFetch(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "alpha", 0), summaryFor("c_2", "bravo", 0), summaryFor("c_3", "charlie", 0),
	}})
	c.focus = focusRail
	c.selected = "c_1"

	ck.NotNil(c.moveSelection(+1), "moving onto a child with no transcript in hand must arm the preview debounce")
	ck.False(c.historyInFlight["c_2"], "moving onto a row issued its fetch immediately; it must wait for the cursor to rest")

	// The cursor has rested: exactly one fetch, and a STALE tick -- armed
	// before the cursor moved on -- must not stack anything behind it.
	c.Update(previewTickMsg{seq: c.selectedSeq})
	ck.False(!c.historyInFlight["c_2"], "the cursor rested on c_2 but its transcript was never fetched")
	if c.moveSelection(+1); c.historyInFlight["c_3"] {
		t.Fatal("moving on issued its fetch before the cursor rested on c_3")
	}
	c.Update(previewTickMsg{seq: c.selectedSeq - 1}) // the tick armed while on c_2
	ck.False(c.historyInFlight["c_3"], "a stale tick acted after the cursor had moved on")
	c.Update(previewTickMsg{seq: c.selectedSeq})
	ck.False(!c.historyInFlight["c_3"], "the rested cursor on c_3 did not fetch its transcript")
}

// A history fetch that fails for the VIEWED child is not a blank pane: the
// subscription opens on the durable log instead -- what the cockpit read before
// GetHistory existed -- and the status line says why the pane may be thinner
// than the conversation. A LATE failure (the cursor moved on while the fetch
// ran) drops the empty session, since nothing on screen asked for it.
func TestFailedHistoryForTheViewedChildFallsBackToTheLog(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 4), summaryFor("c_2", "two", 6),
	}})
	c.focus = focusRail
	c.selected = "c_2"
	c.Update(previewTickMsg{seq: c.selectedSeq})

	c.Update(historyMsg{childID: "c_2", after: 6, err: errors.New("boom")})

	if c.focusChild != "c_2" || c.stopFocus == nil {
		t.Errorf("a failed read left the subscription on %q; the log replay is the fallback, not a blank pane", c.focusChild)
	}
	ck.StrContains(c.status, "history unavailable", "status")

	// A late failure lands for a child nobody is looking at.
	c.moveSelection(-1) // back to c_1
	c.Update(historyMsg{childID: "c_2", after: 6, err: errors.New("boom again")})
	_, ok := c.sessions["c_2"]
	ck.False(ok, "a late failed fetch kept an empty session for a child nothing displays")
}

// While browsing, the status line describes the agent under the cursor -- the
// transcript beside it is that agent's, and a status naming a different child
// would read as its own.
func TestStatusLineDescribesThePreviewedAgent(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_a")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_a", "alpha", 0), summaryFor("c_b", "bravo", 0),
	})
	c.rail.SetFocus("c_a")
	c.Update(railEventMsg{evs: []*rafikiv1.Event{statusEventFor("c_b", "streaming", 4)}})
	c.Update(keyMsg("tab")) // agents pane, cursor on c_a
	defer c.shutdown()

	c.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // preview c_b

	ck.Eq("agent: streaming", c.displayStatus(), "status")
	// And the pane says it is a preview, or it reads as a live conversation.
	ck.StrContains(ansi.Strip(c.View().Content), "preview", "browsing another agent left the status line unmarked; a preview must say so")
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
	assert.NewCollecting(t).Eq("hello", c.ta.Value(), "textarea value")
}

// The ring OWNS the textarea's focus. Left focused while another pane takes
// keys, it blinks a cursor in an input that is ignoring you; left blurred on
// the way back to input, typing stops working again.
func TestOnlyTheInputPaneHoldsTextareaFocus(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	ck.Require().True(c.ta.Focused(), "input pane holds focus at start but the textarea is blurred")
	for _, want := range []struct {
		pane    focusPane
		focused bool
	}{
		{focusRail, false},
		{focusInput, true},
	} {
		c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		ck.Require().Eq(want.pane, c.focus, "after ⇥ focus")
		got := c.ta.Focused()
		ck.Eq(want.focused, got, "pane %v: textarea focused = %v, want", want.pane, got)
	}
}

// Escaping back to input must restore typing, not just the label.
func TestEscapeFromRailRefocusesTheTextarea(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Two agents, so ⇥ really lands on the rail — with none the ring is a
	// one-stop input-only affair and the test would pass vacuously.
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	c.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // → rail
	c.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	ck.Require().Eq(focusInput, c.focus, "esc left focus at")
	ck.True(c.ta.Focused(), "esc returned to the input pane with the textarea still blurred")
}

// ^G was a write-only toggle: it flipped showHelp and View never read it, so
// the key documented in the footer and in `rafiki attach --help` did nothing
// at all.
func TestHelpToggleRendersBindings(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := ansi.Strip(c.View().Content)

	c.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	help := ansi.Strip(c.View().Content)
	ck.Require().NotEq(before, help, "^G changed nothing on screen")
	for _, want := range []string{"steer", "abort", "next pane"} {
		ck.StrContains(help, want, "help overlay is missing")
	}

	c.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	ck.Eq(before, ansi.Strip(c.View().Content), "^G twice did not return to the previous view")
}

// ⇧⏎ is the standard newline in a send-on-⏎ input. It needs an explicit
// binding because BOTH of the textarea's own InsertNewline keys -- enter and
// ^M, which are the same byte -- are taken by Send, so without one a prompt
// can only ever be a single line.
func TestShiftEnterInsertsANewlineAndDoesNotSend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})

	ck.Eq("a\nb", c.ta.Value(), "textarea value")
	ck.Eq("", c.pending, "⇧⏎ sent")
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

	assert.NewCollecting(t).Eq("a\nb", c.ta.Value(), "textarea value")
}

// ⏎ still sends, and a multi-line prompt sends whole.
func TestEnterStillSendsTheWholeMultilinePrompt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	ck.Eq("a\nb", c.pending, "pending")
	ck.Eq("", c.ta.Value(), "textarea not cleared after send")
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
	assert.NewAborting(t).Len(s.Blocks, 2, "history produced %d blocks, want 2", len(s.Blocks))
	if s.Blocks[0].Text != "what is 2+2" || s.Blocks[1].Text != "four" {
		t.Errorf("blocks = %q / %q", s.Blocks[0].Text, s.Blocks[1].Text)
	}
}

// THE hazard in wiring history in. GetHistory stamps
// conversation_message.ordinal; the focus stream resumes from
// conversations.event_log.ordinal. They are unrelated sequences. Folding
// history through Apply would leave a 1217-message conversation with a cursor
// of 1216, and the next subscription on a log holding five events would resume
// past its end and receive nothing, forever, with no error anywhere. The
// cursor history leaves behind is the LOG watermark the fetch was issued
// under -- the resume point -- never anything derived from the history events
// themselves.
func TestHistoryDoesNotMoveTheEventLogCursor(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})

	c.Update(historyMsg{childID: "c_1", after: 4, events: []*rafikiv1.Event{
		historyEvent("c_1", "old prompt", 1216, false),
	}})

	s := c.sessions["c_1"]
	ck.Eq(4, s.Cursor, "cursor = %d, want the log watermark 4; the two ordinal spaces are "+
		"unrelated and history must never set %d", s.Cursor, s.Cursor)

	// A real log event afterwards must still land, and one at or below the
	// watermark must be deduped by it.
	c.applyEvent(userMessageEventFor("c_1", "live", 5))
	ck.Len(s.Blocks, 2, "live event after history produced %d blocks, want 2", len(s.Blocks))
}

// A child with no persisted conversation must replay the whole log rather than
// resuming from its head, or a freshly created agent — whose every event is in
// the log and nowhere else — opens on an empty pane.
func TestEmptyHistoryReplaysTheWholeLog(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 7)}})

	c.Update(historyMsg{childID: "c_1", after: 7, events: nil})
	assert.NewAborting(t).NotNil(c.stopFocus, "empty history must still open the focus stream")
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
	assert.NewCollecting(t).Nil(c.hop("c_1"), "hopping back to a child whose transcript is already loaded must not re-fetch history")
}

// ── the session's single feed ──────────────────────────────────────────────

// The pending echo must clear when the sent message comes back on the focus
// stream. sendFailedMsg clears it on error and this clears it on success —
// there is no third site, so a regression here parks a ⏳ over the input box
// for the rest of the session.
func TestPendingClearsWhenTheMessageComesBack(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil})

	c.sendWith(rafikiv1.SendMode_SEND_MODE_PROMPT, "hello", nil)
	ck.Require().Eq("hello", c.pending, "pending")

	c.applyEvent(userMessageEventFor("c_1", "hello", 5))
	ck.Eq("", c.pending, "the message came back and pending")
}

// The rail and focus streams are independent feeds, and only the focus one
// may advance the session. A rail-delivered agent_status that beats the focus
// stream's user_message to the cockpit used to advance the ordinal cursor
// past a message that was never applied, and Session's dedup ate it: the
// transcript never showed the message and the ⏳ pending echo never cleared —
// through a whole twelve-minute turn and every event after it. Observed
// 2026-09-09: "go for it" was consumed by the daemon in 4ms, the turn ran to
// completion, and the ⏳ sat there the entire time.
func TestRailDeliveryAheadOfFocusDoesNotEatTheUserMessage(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil})

	c.sendWith(rafikiv1.SendMode_SEND_MODE_PROMPT, "go for it", nil)

	// The rail wins the race: its agent_status lands first, one ordinal AHEAD
	// of the message.
	c.Update(railEventMsg{evs: []*rafikiv1.Event{statusEventFor("c_1", "streaming", 148)}})
	c.Update(eventMsg{evs: []*rafikiv1.Event{userMessageEventFor("c_1", "go for it", 147)}})

	assert.NewCollecting(t).Eq("", c.pending, "pending")
	s := c.sessions["c_1"]
	if n := len(s.Blocks); n == 0 || s.Blocks[n-1].Kind != session.KindUser || s.Blocks[n-1].Text != "go for it" {
		t.Errorf("the sent message never reached the transcript; blocks = %d", n)
	}
}

// The cursor belongs to the focus stream alone, even while the rail is
// delivering for the same child. History delivery stamps it with the fetch's
// log watermark (empty history: -1, the replay-from-head point); a rail event
// one ordinal ahead must not advance it past that, or the focus stream's own
// delivery of the same message gets deduped away.
func TestRailEventsDoNotAdvanceTheSessionCursor(t *testing.T) {
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})
	c.Update(historyMsg{childID: "c_1", after: 4, events: nil})

	c.Update(railEventMsg{evs: []*rafikiv1.Event{statusEventFor("c_1", "streaming", 148)}})

	if s := c.sessions["c_1"]; s.Cursor >= 148 {
		t.Errorf("rail delivery advanced the session cursor to %d; the cursor belongs to the focus stream", s.Cursor)
	}
}

// ── the rail feed's plumbing ─────────────────────────────────────────────────

// runCmdDeep runs cmd (which may be a tea.Batch) and returns every message it
// produced. Each sub-command runs in its own goroutine behind a timeout, so a
// wrongly-armed blocking wait fails the test instead of hanging it.
func runCmdDeep(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	bm, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var mu sync.Mutex
	var out []tea.Msg
	var wg sync.WaitGroup
	for _, sub := range bm {
		wg.Add(1)
		go func(sub tea.Cmd) {
			defer wg.Done()
			done := make(chan tea.Msg, 1)
			go func() { done <- sub() }()
			select {
			case m := <-done:
				mu.Lock()
				out = append(out, m)
				mu.Unlock()
			case <-time.After(3 * time.Second):
			}
		}(sub)
	}
	wg.Wait()
	return out
}

// The rail channel must produce railEventMsg, never eventMsg. The message type
// is the only thing telling Update which feed an event came from; the shape of
// the original bug was both channels sharing one waitForEvent that returned
// eventMsg, which made case railEventMsg unreachable and every rail event
// misroute through eventMsg's handler.
func TestWaitForRailEventReturnsRailEventMsg(t *testing.T) {
	ch := make(chan *rafikiv1.Event, 8)
	ch <- statusEventFor("c_1", "streaming", 1)
	ch <- statusEventFor("c_1", "idle", 2)

	msg := waitForRailEvent(ch)()
	rm, ok := msg.(railEventMsg)
	assert.NewAborting(t).True(ok, "waitForRailEvent() = %T, want railEventMsg", msg)
	if len(rm.evs) != 2 || rm.evs[0].GetOrdinal() != 1 || rm.evs[1].GetOrdinal() != 2 {
		t.Fatalf("drained %d events; order and count must survive the drain", len(rm.evs))
	}
}

// The rail waiter must survive its own delivery. This test drives the REAL
// path -- an event written to c.railCh, the waiter the cockpit actually armed,
// Update on what that waiter returns -- because the bug this pins shipped
// green: the tests above constructed railEventMsg directly, and nothing
// exercised the fact that waitForEvent answered eventMsg for BOTH channels.
// The consequence was that the first rail event consumed the rail waiter,
// eventMsg's handler re-armed only the focus side, and every child_spawned
// after the first sat unread in railCh -- the rail stopped tracking children
// coming and going after exactly one event, and the reconnect re-seed
// sentinel was dropped with it.
func TestRailWaiterSurvivesItsOwnDelivery(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 4)}})

	// First rail event through the real channel.
	c.railCh <- statusEventFor("c_1", "streaming", 148)
	delivered := runCmdDeep(t, waitForRailEvent(c.railCh))
	ck.Len(delivered, 1, "first delivery produced %d messages, want 1", len(delivered))
	rm, ok := delivered[0].(railEventMsg)
	if !ok {
		t.Fatalf("rail delivery arrived as %T, want railEventMsg", delivered[0])
	}

	// Feeding it through Update must re-arm a rail waiter that still reads the
	// RAIL channel -- the second event must come back as railEventMsg too.
	_, cmd := c.Update(rm)
	ck.NotNil(cmd, "Update returned no command: the rail waiter was consumed and never re-armed")
	c.railCh <- statusEventFor("c_1", "idle", 149)
	second := runCmdDeep(t, cmd)
	var gotRail bool
	for _, m := range second {
		em, ok := m.(eventMsg)
		if ok {
			t.Fatalf("the re-armed waiter delivered %T carrying %d events; rail events must arrive as railEventMsg", m, len(em.evs))
		}
		if rm2, ok := m.(railEventMsg); ok {
			gotRail = true
			ck.False(len(rm2.evs) != 1 || rm2.evs[0].GetOrdinal() != 149, "re-armed waiter delivered %v, want the second event", rm2.evs)
			// Fold it the way Update would: the rail must see it, the session
			// must not.
			c.Update(rm2)
		}
	}
	if !gotRail {
		t.Fatalf("the re-armed waiter never delivered a second rail event; got %d messages", len(second))
	}

	if n, _ := c.rail.Get("c_1"); n.RailCursor != 149 {
		t.Errorf("rail cursor = %d, want 149", n.RailCursor)
	}
	if s := c.sessions["c_1"]; s.HasCursor {
		t.Errorf("rail delivery advanced the session cursor to %d", s.Cursor)
	}
}

// The reconnect sentinel (streams.StartRail's nil) must arrive on the rail
// channel as a railEventMsg carrying nil, and must arm the re-seed -- it used
// to be dropped on the eventMsg path, so children spawned during a disconnect
// never appeared.
func TestNilSentinelThroughTheRealRailChannelRequestsAReSeed(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()

	c.railCh <- nil
	delivered := runCmdDeep(t, waitForRailEvent(c.railCh))
	ck.Len(delivered, 1, "sentinel delivery produced %d messages, want 1", len(delivered))
	rm, ok := delivered[0].(railEventMsg)
	if !ok || len(rm.evs) != 1 || rm.evs[0] != nil {
		t.Fatalf("sentinel arrived as %T, want railEventMsg carrying nil", delivered[0])
	}

	c.Update(rm)
	// maybeReseed consumes the request synchronously: reseeding flips to
	// reseedInFlight and a ListChildren rides out with the returned command.
	ck.False(!c.reseeding && !c.reseedInFlight, "nil sentinel did not arm the re-seed")
}

// ── interaction ──────────────────────────────────────────────────────────────

// A single stray ^C must not throw away an attached session. The key arms, the
// repeat quits — and anything in between disarms, so a ^C now and a ^C a minute
// later are two intentions rather than a quit.
func TestQuitTakesTwoPresses(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, k := range []tea.KeyPressMsg{
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: 'd', Mod: tea.ModCtrl},
	} {
		c := newTestCockpit("c_1")
		c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

		if _, cmd := c.Update(k); cmd != nil {
			t.Fatalf("%v: one press quit; it must arm and wait for the repeat", k)
		}
		ck.Require().False(c.quitting, "%v: one press set quitting", k)
		ck.NotEq("", c.notice, "%v: an armed quit must say so on screen", k)
		_, cmd := c.Update(k)
		ck.Require().NotNil(cmd, "%v: the repeat must quit", k)
		ck.True(c.quitting, "%v: the repeat must set quitting", k)
	}
}

func TestAnotherKeyDisarmsTheQuit(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	c.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	ck.Require().Nil(cmd, "a keystroke between the two presses must disarm the quit")
	ck.False(c.quitting, "a disarmed quit still quit")
}

// A transcript shorter than the pane is bottom-anchored, so the newest line
// sits on the row it will occupy once the conversation is long. A viewport
// renders from the top by default, which made a new conversation start at the
// ceiling and crawl down.
func TestShortTranscriptIsBottomAnchored(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	p := c.pane("c_1")
	c.syncViewport(p, []string{"only line"})

	view := strings.Split(ansi.Strip(p.vp.View()), "\n")
	ck.Require().GreaterOrEqual(2, len(view), "viewport rendered")
	ck.Eq("", strings.TrimSpace(view[0]), "short transcript starts at the TOP of the pane; want it padded to the bottom:\n%q", view[0])
	last := strings.TrimSpace(view[len(view)-1])
	ck.Eq("only line", last, "last pane row")
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
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	ck.Require().Eq(focusInput, c.focus, "PgUp moved focus; it must scroll in place")
	ck.Less(before, p.vp.YOffset(), "PgUp did not scroll: offset")
	ck.Eq("", c.ta.Value(), "PgUp reached the textarea")
}

// ↑ is SHARED. With a single-line prompt the cursor has nowhere to go, so the
// key falls through to the transcript.
func TestUpScrollsWhenTheCursorCannotMove(t *testing.T) {
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.NewCollecting(t).Less(before, p.vp.YOffset(), "↑ on a single-line prompt did not scroll: offset")
}

// ...and the textarea keeps it whenever the cursor CAN move, so a multi-line
// prompt is still editable.
func TestUpStaysInTheTextareaWhenItHasSomewhereToGo(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	before := p.vp.YOffset()

	c.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	row := c.ta.Line()
	ck.Require().NotEq(0, row, "fixture is vacuous: the prompt is not multi-line")

	c.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	ck.Eq(row-1, c.ta.Line(), "↑ did not move the textarea cursor: row %d →", row)
	ck.Eq(before, p.vp.YOffset(), "↑ scrolled the transcript while the cursor still had a line above it")
}

// home/end are top/bottom, FROM THE INPUT PANE. They were transcript-pane-only
// and so appeared not to work at all: with PgUp/PgDn reading from the input
// box nobody tabbed away, and a key that only works in a pane you never visit
// is a key that does not work.
func TestHomeAndEndJumpTheTranscript(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	p := paneWithContent(t, c)
	c.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	ck.Eq(0, p.vp.YOffset(), "home left offset at")
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	ck.True(p.vp.AtBottom(), "end did not reach the bottom")
}

// Naming the focused pane in a grey footer line was not enough: that is not
// where the eye is, so finding it meant cycling ⇥ and watching for a response.
func TestTheFocusedPaneIsMarkedOnScreen(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})

	seen := map[focusPane]string{}
	for _, want := range []focusPane{focusRail, focusInput} {
		c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		ck.Require().Eq(want, c.focus, "focus")
		raw := c.View().Content
		ck.StrContains(ansi.Strip(raw), " "+want.String()+" ", "%v: the footer badge does not name the pane", want)
		seen[want] = raw
	}
	// The panes must look DIFFERENT, not merely be named differently: the
	// badge alone is the thing that was already there and was missed.
	ck.NotEq(seen[focusInput], seen[focusRail], "rail and input focus render identically")
}

// "↓ more below" answered whether you were at the bottom, never where you
// were. The readout is bottom-RIGHT and reports the CONTENT's length: a short
// transcript is padded to bottom-anchor it, and the viewport counts that
// padding as real, so asking it would report 12/12 for a one-line conversation.
//
// At the bottom the readout is hidden entirely -- that slot belongs to
// contextReadout there -- and it reappears only once you scroll back.
func TestScrollPositionReportsWhereYouAre(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	paneWithContent(t, c) // 200 lines, pane is 24 tall

	ck.Eq("", c.scrollPosition(), "at the bottom the readout")

	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	got := c.scrollPosition()
	if !strings.HasPrefix(got, "↓") {
		t.Errorf("scrolled up, readout = %q, want it to mark more below", got)
	}
	ck.NotStrContains(got, "200/200", "readout did not move after PgUp")
	ck.StrContains(got, "/200", "readout lost the total")
}

// A transcript shorter than the pane is padded to sit at the bottom -- and a
// short transcript is ALWAYS at the bottom, so the readout is hidden for it
// unconditionally now rather than needing to prove it ignores the padding
// rows in its count.
func TestScrollPositionHiddenForShortPaddedTranscript(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.syncViewport(c.pane("c_1"), []string{"one", "two"})

	assert.NewCollecting(t).Eq("", c.scrollPosition(), "readout")
}

// It is right-aligned so it does not move when the key hints do.
func TestScrollPositionIsRightAligned(t *testing.T) {
	c := newTestCockpit("c_1")
	paneWithContent(t, c)
	c.Update(tea.KeyPressMsg{Code: tea.KeyPgUp}) // scroll back: hidden at the bottom now
	view := ansi.Strip(c.View().Content)
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	last := lines[len(lines)-1]

	assert.NewCollecting(t).True(strings.HasSuffix(strings.TrimRight(last, " "), "%"), "footer does not end with the position readout:\n%q", last)
}

// ⇥ is a TOGGLE, not a three-stop cycle. The transcript pane existed to give
// the viewport its own keymap while a textarea held every plausible scroll key;
// the input pane scrolls directly now, so the third stop bought nothing and
// cost a press on every agent switch — the move made most often.
func TestFocusRingIsATwoStopToggle(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	ck.Eq(focusRail, c.focus, "first ⇥ →")
	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	ck.Eq(focusInput, c.focus, "second ⇥ →")
}

// With the rail hidden there is nothing to switch to, and ⇥ must leave focus
// where it is rather than parking it on a pane that no longer exists.
func TestTabIsInertWhenTheRailIsHidden(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}) // hide the rail

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	ck.Eq(focusInput, c.focus, "focus")
	ck.True(c.ta.Focused(), "⇥ with the rail hidden left the textarea blurred")
}

// ── the one-agent rail: ^R peeks it ──────────────────────────────────────
//
// Below two rows the rail is hidden by DEFAULT (railCols), which made ^R read
// as a dead key with exactly one agent — and the spawn form is only reachable
// via `n` on the rail, so a single-agent cockpit could not grow a second one.
// An explicit request now outranks the default look the same way the ⇥ peek
// always has: ^R reveals the one-row rail focused, and esc/⏎/^R puts it back.

func oneAgentCockpit(t *testing.T) *Cockpit {
	t.Helper()
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "only", 0)})
	return c
}

func ctrlR(c *Cockpit) {
	c.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
}

func TestCtrlRPeeksTheOneRowRail(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()

	// The default look first: a single-agent session is a full-width
	// conversation, the state a fresh `create` opens in.
	ck.Require().Eq(0, c.railCols(), "rail is drawn with one agent before anyone asked for it")

	ctrlR(c)

	ck.NotEq(0, c.railCols(), "^R with one agent did not reveal the rail")
	ck.Eq(focusRail, c.focus, "focus")
	ck.True(c.railPeek, "the reveal was not recorded as a peek, so leaving the rail cannot put it back")
	// The "▶ " focus cursor is rendered by nothing but the rail (the status
	// line names the agent too, so the name alone would not prove the rail
	// drew). Glyph-agnostic: the glyph sits between cursor and name.
	ck.StrContains(ansi.Strip(c.View().Content), "▶ ", "the peeked rail did not render its row under the focus cursor:\n%s", c.View().Content)
}

func TestSpawnFormOpensWhilePeeked(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()
	ctrlR(c)

	// `n` is rail-local, so this is the path the peek exists to open: with one
	// agent there is no other keyboard route to the spawn form.
	c.Update(keyMsg("n"))

	ck.Require().NotNil(c.form, "n on the peeked rail did not open the spawn form")
	// A modal still takes the whole panel — the peek survives underneath and
	// the rail comes back when the form closes.
	ck.Eq(0, c.railCols(), "the rail is drawn behind the create form")
	c.Update(keyMsg("esc"))
	ck.Nil(c.form, "esc did not close the form")
	ck.NotEq(0, c.railCols(), "the peeked rail did not come back when the form closed")
}

func TestLeavingThePeekedRailPutsItBack(t *testing.T) {
	for _, key := range []string{"esc", "enter"} {
		t.Run(key, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c := oneAgentCockpit(t)
			defer c.shutdown()
			ctrlR(c)

			c.Update(keyMsg(key))

			ck.Eq(0, c.railCols(), "%s from the peeked rail left it drawn", key)
			ck.Eq(focusInput, c.focus, "focus = %v after %s, want input", c.focus, key)
			ck.True(c.ta.Focused(), "%s returned to the input pane with the textarea blurred", key)
		})
	}
}

func TestCtrlRWhilePeekedHidesItAgain(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()
	ctrlR(c)
	ctrlR(c)

	ck.Eq(0, c.railCols(), "a second ^R did not hide the peeked rail")
	ck.Eq(focusInput, c.focus, "focus")
	ck.True(c.ta.Focused(), "hiding the rail while it holds focus left the textarea blurred")
}

// ⇥ with one agent is the same round trip the multi-agent rail already runs:
// it peeks rather than doing nothing (or stranding focus on an invisible
// pane, which the old ring allowed — the rail was focusable below two rows
// even though nothing rendered), a second ⇥ returns focus to input, and
// leaving the rail puts it back.
func TestTabPeeksTheOneRowRail(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()

	c.Update(keyMsg("tab"))
	ck.Require().Eq(focusRail, c.focus, "first ⇥ →")
	ck.Require().NotEq(0, c.railCols(), "⇥ moved focus onto a rail that is not drawn")

	c.Update(keyMsg("tab"))
	ck.Require().Eq(focusInput, c.focus, "second ⇥ →")

	// Leaving the rail (esc/⏎ go through leaveRail) re-hides it. Go back onto
	// the rail first — the peek survives a focus move, exactly like a
	// multi-agent peek.
	c.Update(keyMsg("tab"))
	c.Update(keyMsg("esc"))
	ck.Eq(0, c.railCols(), "esc after the peek did not put the rail back")
	ck.Eq(focusInput, c.focus, "focus")
}

// A peek cannot outlive the last row: closing the peeked row out of the rail
// must drop the column entirely rather than keep drawing an empty one beside
// the transcript — and must not strand pane focus on the list that stopped
// existing, the same trap the focus ring exists to close.
func TestPeekEndsWithTheLastRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()
	ctrlR(c)
	ck.Require().NotEq(0, c.railCols(), "the peek did not reveal the rail to begin with")
	ck.Require().Eq(focusRail, c.focus, "the peek did not focus the rail")

	c.applyClosed(closedMsg{childID: "c_1", name: "c_1"})

	ck.Require().Eq(0, c.rail.Len(), "rail still holds")
	ck.Eq(0, c.railCols(), "an empty rail column is still drawn after its only row closed")
	ck.Eq(focusInput, c.focus, "focus")
	ck.True(c.ta.Focused(), "the textarea is still blurred after the rail emptied under focus")
	ck.False(c.railPeek, "the peek survived its row; it would resurrect the rail for the next single agent")
}

// Closing the last agent leaves nothing to view -- the same state a bare
// attach with no children lands in, and it gets the same answer. Driven
// through Update so the form's catalog fetch is not dropped on the floor.
func TestClosingTheLastAgentOpensCreateForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := oneAgentCockpit(t)
	defer c.shutdown()

	c.Update(closedMsg{childID: "c_1", name: "c_1"})

	ck.Require().NotNil(c.form, "closing the last agent did not open the create form")
	ck.Eq(0, c.railCols(), "an empty rail should not be drawn behind the create form")
}

// Closing one of several is not that state: the rail still has something on it.
func TestClosingOneOfSeveralAgentsLeavesTheFormShut(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	exitChild(c, "c_1")
	c.Update(closedMsg{childID: "c_1", name: "c_1"})

	assert.NewCollecting(t).Nil(c.form, "the create form opened with an agent still on the rail")
}

// esc out of the form on an empty rail must not strand you: ⇥ is how you get
// to the rail everywhere else, and with no rows the rail's only job is the
// form, so ⇥ goes there rather than being a dead key.
func TestTabOnAnEmptyRailReopensCreateForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := bareCockpit(t)
	c.Update(seedMsg{children: nil})
	defer c.shutdown()
	c.Update(keyMsg("esc"))
	ck.Require().Nil(c.form, "esc did not close the create form")

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	ck.NotNil(c.form, "⇥ on an empty rail did not reopen the create form")
}

// Zero agents: ^R goes straight to the create form. There is nothing to peek
// at, and a rail with no rows is not a state worth entering -- it is the
// thing the form exists to fix.
func TestCtrlRWithNoAgentsOpensCreateForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := bareCockpit(t)
	defer c.shutdown()

	ctrlR(c)

	ck.NotNil(c.form, "^R with no agents did not open the create form")
	ck.Eq(0, c.railCols(), "^R revealed a rail with no agents in it")
}

// ⇥ reveals a rail the USER hid with two agents, the other half of the
// reveal branch cyclePane now computes from railVisible rather than the bare
// flag — hiding is about screen space, not about giving up agent switching.
func TestTabRevealsTheHiddenRailWithTwoAgents(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	c.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}) // hide the rail
	ck.Require().Eq(0, c.railCols(), "the rail did not hide to begin with")

	c.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	ck.Require().Eq(focusRail, c.focus, "⇥ over a hidden rail →")
	ck.NotEq(0, c.railCols(), "⇥ focused a rail that is still hidden")
	ck.True(c.railPeek, "the ⇥ reveal was not recorded as a peek")
}

// Two or more agents: the ^R toggle keeps its existing flip semantics — the
// peek machinery is for the rail the DEFAULT look hides, not the one the user
// chose to collapse.
func TestCtrlRTogglesWithTwoAgentsAsBefore(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	defer c.shutdown()

	ck.Require().NotEq(0, c.railCols(), "the rail is not drawn with two agents to begin with")

	ctrlR(c)
	ck.Eq(0, c.railCols(), "^R did not hide the rail with two agents")
	ck.False(c.railPeek, "hiding recorded a peek; the peek flag belongs to reveals, not to a collapse")

	ctrlR(c)
	ck.NotEq(0, c.railCols(), "a second ^R did not restore the rail")
}

// ── where a bare `rafiki attach` lands ───────────────────────────────────────

func bareCockpit(t *testing.T) *Cockpit {
	t.Helper()
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1"})
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return c
}

// Nothing is focused, so the input box can do nothing — Send returns early on
// an empty child. Opening on it puts the cursor in a box that cannot accept
// work and hides the one thing there is to do.
func TestBareAttachLandsOnAgentSelection(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := bareCockpit(t)
	ck.Require().Eq(focusRail, c.focus, "focus")
	ck.False(c.ta.Focused(), "the textarea is focused while no agent is selected")

	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	}})
	ck.Require().Eq(focusRail, c.focus, "focus")
	// The cursor must already be somewhere, or ↑/↓ and ⏎ need a priming press.
	ck.NotEq("", c.selected, "no rail row is under the cursor; ⏎ would open nothing")
	defer c.shutdown()
}

// One child is not a choice: picking from a list of one is a keystroke that
// carries no information.
func TestBareAttachWithOneChildOpensIt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := bareCockpit(t)
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "only", 0)}})
	defer c.shutdown()

	ck.Eq("c_1", c.focused(), "focused child")
	ck.Eq(focusInput, c.focus, "focus")
	ck.True(c.ta.Focused(), "landed on the input pane with the textarea blurred")
}

// No children is not a choice either, and there is nothing to view — land
// straight in the create form instead of stranding focus on an empty pane.
func TestBareAttachWithNoChildrenOpensCreateForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := bareCockpit(t)
	c.Update(seedMsg{children: nil})
	defer c.shutdown()

	ck.Require().NotNil(c.form, "no children seeded, want the create form open")
	ck.Eq(0, c.railCols(), "an empty rail should not be drawn behind the create form")
}

// The confirmation is built from the binding's help text, so naming only ^C
// after someone pressed ^D reads as the cockpit having missed the key.
func TestQuitConfirmationNamesBothKeys(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: 'd', Mod: tea.ModCtrl},
	} {
		c := newTestCockpit("c_1")
		c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		c.Update(k)
		for _, want := range []string{"^C", "^D"} {
			assert.NewCollecting(t).StrContains(c.notice, want, "after %v the notice is %q, want it to name", k, c.notice)
		}
	}
}

// ── input box ────────────────────────────────────────────────────────────────

// A fixed three rows wasted two on the common one-line prompt and hid
// everything past the third on a long one.
func TestInputGrowsWithThePromptAndStops(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	start := c.ta.Height()
	body := c.bodyHeight()

	for i := 0; i < 4; i++ {
		c.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	}
	grown := c.ta.Height()
	ck.Require().Greater(start, grown, "input height")
	// The transcript yields exactly the rows the box took, or they overlap.
	got, want := c.bodyHeight(), body-(grown-start)
	ck.Eq(want, got, "body height")

	for i := 0; i < 40; i++ {
		c.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	}
	ck.LessOrEqual(maxInputHeight, c.ta.Height(), "input grew to")
}

// A big paste is a file, not something you meant to type. Unrolling one buries
// the conversation and pins the box at its cap.
func TestLargePasteFoldsToAToken(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	big := strings.Repeat("a line\n", 50)

	c.Update(tea.PasteMsg{Content: big})

	shown := c.ta.Value()
	ck.NotStrContains(shown, "a line", "a 50-line paste was unrolled into the box: %q", truncate(shown, 60))
	ck.StrContains(shown, "51 lines", "the token must say how much it stands for; got")
	// ...and the full text is what actually gets sent.
	got := c.expandPastes(shown)
	ck.Eq(big, got, "expansion did not restore the paste (%d chars vs %d)", len(got), len(big))
}

// Small pastes are what you meant to type and belong in the box.
func TestSmallPasteIsInsertedWhole(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.PasteMsg{Content: "one\ntwo"})
	assert.NewCollecting(t).Eq("one\ntwo", c.ta.Value(), "small paste")
}

// Pasting the same content again is how you say you actually wanted to see it.
func TestPastingTwiceInsertsTheFullText(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	big := strings.Repeat("a line\n", 50)

	c.Update(tea.PasteMsg{Content: big})
	c.Update(tea.PasteMsg{Content: big})

	assert.NewCollecting(t).StrContains(c.ta.Value(), "a line", "pasting the same content twice must insert it in full")
}

// The prompt that leaves is the full text, and the tokens leave with it.
func TestSendExpandsAndClearsPastes(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	big := strings.Repeat("a line\n", 50)

	c.Update(tea.PasteMsg{Content: big})
	c.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	ck.StrContains(c.pending, "a line", "the sent prompt kept the token instead of the text: %q", truncate(c.pending, 60))
	ck.Empty(c.pastes, "pastes outlived the prompt they belonged to")
}

// A terminal sends CARRIAGE RETURNS for the line breaks inside a bracketed
// paste, and the paste buffer keeps them verbatim — so the content contains no
// \n at all, counting by \n returned 1 for a forty-line paste, and every real
// ⌘V of multi-line text sailed under the threshold and unrolled into the box.
func TestCarriageReturnPasteIsCountedAndFolded(t *testing.T) {
	for _, tc := range []struct{ name, sep string }{
		{"CR", "\r"}, {"CRLF", "\r\n"}, {"LF", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c := newTestCockpit("c_1")
			c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			var body strings.Builder
			for i := 0; i < 40; i++ {
				if i > 0 {
					body.WriteString(tc.sep)
				}
				body.WriteString("line")
			}

			c.Update(tea.PasteMsg{Content: body.String()})

			shown := c.ta.Value()
			ck.Require().StrContains(shown, "40 lines", "a 40-line %s paste was not folded; box holds %q", tc.name, truncate(shown, 60))
			// What gets SENT must have real newlines: an agent should not
			// receive a prompt whose line breaks are carriage returns.
			expanded := c.expandPastes(shown)
			ck.NotStrContains(expanded, "\r", "carriage returns survived into the sent prompt")
			ck.Eq(39, strings.Count(expanded, "\n"), "expanded paste has")
		})
	}
}

// Lines alone was not enough: one wide line is a single line of many thousands
// of characters and pinned the box at its cap.
func TestWidePasteFoldsEvenOnOneLine(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	wide := strings.Repeat("x", pasteCharThreshold+1)

	c.Update(tea.PasteMsg{Content: wide})

	shown := c.ta.Value()
	ck.NotStrContains(shown, wide, "a %d-character single-line paste was not folded", len(wide))
	ck.StrContains(shown, "chars", "a one-line paste must be measured in chars; got")
	ck.Eq(wide, c.expandPastes(shown), "expansion did not restore the wide paste")
}

func TestPasteUnderBothBoundsIsInsertedWhole(t *testing.T) {
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	small := strings.Repeat("y", pasteCharThreshold-1)
	c.Update(tea.PasteMsg{Content: small})
	assert.NewCollecting(t).Eq(small, c.ta.Value(), "a paste under both bounds must be inserted whole")
}

// ^U is the shell reflex for "kill the line", and the textarea's own ^U
// (DeleteBeforeCursor) already clears the whole prompt in the common case of
// one line with the cursor at the end — this widens it to the whole box.
func TestClearInputEmptiesTheBoxAndItsPastes(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.PasteMsg{Content: strings.Repeat("a line\r", 40)})
	c.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	c.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	ck.Require().False(c.ta.Value() == "" || len(c.pastes) == 0, "fixture is vacuous: nothing to clear")

	c.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})

	ck.Eq("", c.ta.Value(), "box still holds")
	// The folded pastes go with it: their tokens are what referred to them,
	// and leaving the text behind would attach it to a later prompt that
	// happened to contain a matching token.
	ck.Empty(c.pastes, "folded pastes outlived the input that referenced them")
	ck.False(c.quitting, "^U quit the cockpit")
}

// ── attachments ──────────────────────────────────────────────────────────────

// A terminal never sends image DATA through a bracketed paste. Dragging a file
// pastes its PATH, which is the only way an image reaches the input box today.
func TestPastingAnImagePathStagesIt(t *testing.T) {
	ck := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	ck.Require().NoError(os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nfake"), 0o600))
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	c.Update(tea.PasteMsg{Content: path})

	ck.Require().Len(c.attachments, 1, "staged %d attachments, want 1", len(c.attachments))
	ck.Eq("image/png", c.attachments[0].mediaType, "media type")
	ck.StrContains(c.ta.Value(), "shot.png", "the box should name the attachment; got")
	// The path itself must not be left in the prompt as text.
	ck.NotStrContains(c.ta.Value(), dir, "the raw path leaked into the prompt")
}

// Ordinary text that merely looks path-shaped is untouched, and so is a path
// to something that is not there.
func TestNonImagePastesAreUnaffected(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	c.Update(tea.PasteMsg{Content: "/no/such/file.png"})
	ck.Empty(c.attachments, "a path to a missing file must not stage an attachment")
	ck.Eq("/no/such/file.png", c.ta.Value(), "a missing path must land as plain text; got")

	c.ta.Reset()
	c.Update(tea.PasteMsg{Content: "just some prose"})
	if len(c.attachments) != 0 || c.ta.Value() != "just some prose" {
		t.Errorf("prose was mangled: attachments=%d value=%q", len(c.attachments), c.ta.Value())
	}
}

// An attachment alone is a message: a screenshot with nothing to say still has
// something to say. Requiring text would make it unsendable.
func TestAnAttachmentAloneCanBeSent(t *testing.T) {
	ck := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "only.png")
	ck.Require().NoError(os.WriteFile(path, []byte("\x89PNGdata"), 0o600))
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.PasteMsg{Content: path})
	c.ta.Reset() // no text at all, just the attachment

	_, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ck.Require().NotNil(cmd, "an attachment with no text must still send")
	ck.Empty(c.attachments, "attachments outlived the prompt they rode on")
}

// ^U clears staged attachments too — the token that referred to them is gone.
func TestClearInputDropsStagedAttachments(t *testing.T) {
	ck := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "x.png")
	ck.Require().NoError(os.WriteFile(path, []byte("\x89PNGdata"), 0o600))
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(tea.PasteMsg{Content: path})

	c.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	ck.Empty(c.attachments, "^U left attachments staged with no token referring to them")
}

// The toggle must reach the renderer AND invalidate the pane cache. Flipping
// a flag that paneSig does not carry changes nothing on screen.
func TestExpandArgsTogglesAndInvalidates(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestCockpit("c_1")
	before := c.expandArgs
	c.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	ck.NotEq(before, c.expandArgs, "^O did not toggle expandArgs")
	var sig paneSig
	sig.expandArgs = c.expandArgs
	ck.True(sig.expandArgs, "paneSig has no expandArgs field")
}

// A prompt sent to a busy agent used to sit in the inbox until the turn
// settled, because ModePrompt is busy-gated. Typing at a working agent should
// reach it at the next opportunity.
func TestEnterSteersABusyAgent(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   rafikiv1.SendMode
	}{
		{"streaming", rafikiv1.SendMode_SEND_MODE_STEER},
		{"tool_running", rafikiv1.SendMode_SEND_MODE_STEER},
		{"compacting", rafikiv1.SendMode_SEND_MODE_STEER},
		{"idle", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{"spawning", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{"blocked_ui", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{"exited", rafikiv1.SendMode_SEND_MODE_PROMPT},
		{"", rafikiv1.SendMode_SEND_MODE_PROMPT},
	} {
		got := sendModeFor(rafikiv1.SendMode_SEND_MODE_PROMPT, tc.status)
		assert.NewCollecting(t).Eq(tc.want, got, "status %q: got %v, want", tc.status, got)
	}
}

// An explicit steer stays a steer, and abort is never rewritten.
func TestExplicitModesAreNotRewritten(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(rafikiv1.SendMode_SEND_MODE_STEER, sendModeFor(rafikiv1.SendMode_SEND_MODE_STEER, "idle"), "an explicit steer at an idle agent became")
	c.Eq(rafikiv1.SendMode_SEND_MODE_ABORT, sendModeFor(rafikiv1.SendMode_SEND_MODE_ABORT, "streaming"), "abort was rewritten to")
}

// ^L must force a real repaint, not a cached one. The pane skips rebuilding
// when its signature is unchanged, so clearing the screen without
// invalidating leaves it blank until the next event.
func TestRedrawInvalidatesThePaneCache(t *testing.T) {
	c := assert.NewCollecting(t)
	p := &paneState{renderer: newRenderer(), atBottom: true}
	s := session.New("c1")
	s.Blocks = []session.Block{{Kind: session.KindUser, Text: "hello", Final: true}}
	s.Finalized = 1

	c.Require().NotNil(p.linesFor(s, 80, 24, false), "first render returned nil")
	c.Require().Nil(p.linesFor(s, 80, 24, false), "second render should have been a cache hit")
	p.invalidate()
	c.NotNil(p.linesFor(s, 80, 24, false), "invalidate did not force a rebuild")
}

// No polling. The cockpit already sees every tool call, so a completed task
// mutation is the refresh trigger.
func TestTaskToolsTriggerARefresh(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, name := range []string{"task_add", "task_update", "task_drop"} {
		c.True(isTaskTool(name), "%s must trigger a task refresh", name)
	}
	for _, name := range []string{"bash", "read", "agent_spawn", ""} {
		c.False(isTaskTool(name), "%s must not trigger a task refresh", name)
	}
}
