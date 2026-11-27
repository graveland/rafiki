// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// at builds a local-time instant so the expected strings do not depend on the
// machine's zone.
func local(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.Local)
}

func TestDividerLabel(t *testing.T) {
	now := local(2026, 11, 26, 18, 0, 0)
	for _, tc := range []struct {
		name      string
		at, prev  time.Time
		want      string
		wantShown bool
	}{
		{"first block today", local(2026, 11, 26, 14, 32, 0), time.Time{}, "14:32", true},
		{"first block yesterday", local(2026, 11, 25, 14, 32, 0), time.Time{}, "Wed Nov 25 14:32 · 1d ago", true},
		{"first block last week", local(2026, 11, 20, 9, 5, 0), time.Time{}, "Fri Nov 20 09:05 · 6d ago", true},
		{"first block weeks ago", local(2026, 10, 20, 9, 5, 0), time.Time{}, "Tue Oct 20 09:05 · 5w ago", true},
		{"first block months ago", local(2026, 3, 2, 9, 5, 0), time.Time{}, "Mon Mar 2 09:05 · 8mo ago", true},
		{"first block last year", local(2025, 6, 2, 9, 5, 0), time.Time{}, "Mon Jun 2 2025 09:05 · 1y ago", true},
		{"quiet pause today", local(2026, 11, 26, 14, 40, 0), local(2026, 11, 26, 14, 32, 0), "14:40", true},
		{"quiet pause on an old day still says which day", local(2026, 11, 20, 14, 40, 0), local(2026, 11, 20, 14, 32, 0), "Fri Nov 20 14:40 · 6d ago", true},
		{"ordinary rhythm", local(2026, 11, 26, 14, 36, 0), local(2026, 11, 26, 14, 32, 0), "", false},
		{"exactly the interval", local(2026, 11, 26, 14, 37, 0), local(2026, 11, 26, 14, 32, 0), "14:37", true},
		{"midnight crossed within the threshold", local(2026, 11, 26, 0, 1, 0), local(2026, 11, 25, 23, 58, 0), "Thu Nov 26 00:01", true},
	} {
		got, shown := dividerLabel(tc.at, tc.prev, now)
		assert.NewCollecting(t).Eq(tc.wantShown, shown, "%s: shown", tc.name)
		if tc.wantShown {
			assert.NewCollecting(t).Eq(tc.want, got, "%s: label", tc.name)
		}
	}
}

func TestAgeLabel(t *testing.T) {
	now := local(2026, 11, 26, 0, 30, 0)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{local(2026, 11, 26, 0, 1, 0), ""},
		{local(2026, 11, 25, 23, 59, 0), "1d ago"},
		{local(2026, 11, 13, 12, 0, 0), "13d ago"},
		{local(2026, 11, 12, 12, 0, 0), "2w ago"},
		{local(2026, 10, 1, 12, 0, 0), "8w ago"},
		{local(2026, 9, 27, 12, 0, 0), "2mo ago"},
		{local(2025, 11, 26, 12, 0, 0), "1y ago"},
	} {
		assert.NewCollecting(t).Eq(tc.want, ageLabel(tc.at, now), "age of %v", tc.at)
	}
}

func TestTurnDurStr(t *testing.T) {
	for in, want := range map[time.Duration]string{
		840 * time.Millisecond:                "840ms",
		12400 * time.Millisecond:              "12.4s",
		125 * time.Second:                     "2m05s",
		59*time.Minute + 59*time.Second:       "59m59s",
		time.Hour + 4*time.Minute + time.Hour: "2h04m",
	} {
		assert.NewCollecting(t).Eq(want, turnDurStr(in), "turnDurStr(%v)", in)
	}
}

func timedBlocks(base time.Time) []session.Block {
	return []session.Block{
		{Kind: session.KindUser, At: base, Text: "first question", Final: true},
		{Kind: session.KindAssistant, At: base.Add(20 * time.Second), Text: "first answer", Final: true,
			TurnDuration: 12400 * time.Millisecond},
		{Kind: session.KindUser, At: base.Add(20 * time.Minute), Text: "second question", Final: true},
	}
}

var dividerRow = regexp.MustCompile(`(?m)^\s*── .+ ──$`)

// timedRenderer is a renderer whose clock stands at `now`.
func timedRenderer(mode timeMode, now time.Time) *renderer {
	r := newRenderer()
	r.timeMode = mode
	r.clock = func() time.Time { return now }
	return r
}

func renderTimed(mode timeMode, base, now time.Time) string {
	r := timedRenderer(mode, now)
	blocks := timedBlocks(base)
	return ansi.Strip(strings.Join(r.Lines(blocks, len(blocks), 80), "\n"))
}

func TestPeriodicModeDrawsDividersAndTheTurnDuration(t *testing.T) {
	ck := assert.NewCollecting(t)
	base := local(2026, 11, 26, 14, 32, 5)
	out := renderTimed(timePeriodic, base, local(2026, 11, 26, 18, 0, 0))

	// Above the first block, and above the one after a 20 minute pause; not
	// above the answer 20 seconds after its question.
	ck.EqDeep([]string{"── 14:32 ──", "── 14:52 ──"}, trimAll(dividerRow.FindAllString(out, -1)),
		"dividers in:\n%s", out)
	ck.StrContains(out, "12.4s", "the turn duration is missing:\n")
	ck.NotStrContains(out, "14:32:05", "gaps mode stamped a message:\n")
}

// Opening an old conversation is the common case: every divider says which day
// it is and how long ago, because nothing else on screen does.
func TestPeriodicModeOnAnOldConversationNamesTheDayAndAge(t *testing.T) {
	ck := assert.NewCollecting(t)
	base := local(2026, 11, 20, 14, 32, 5)
	out := renderTimed(timePeriodic, base, local(2026, 11, 26, 18, 0, 0))

	ck.EqDeep([]string{"── Fri Nov 20 14:32 · 6d ago ──", "── Fri Nov 20 14:52 · 6d ago ──"},
		trimAll(dividerRow.FindAllString(out, -1)), "dividers in:\n%s", out)
}

func trimAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

func TestOffModeDrawsNoTiming(t *testing.T) {
	ck := assert.NewCollecting(t)
	out := renderTimed(timeOff, local(2026, 11, 26, 14, 32, 5), local(2026, 11, 26, 18, 0, 0))
	ck.NotStrContains(out, "──", "off mode drew a divider or footer:\n")
	ck.NotStrContains(out, "12.4s", "off mode drew the turn duration:\n")
}

func TestAllModeStampsEveryBlockRightAligned(t *testing.T) {
	ck := assert.NewCollecting(t)
	base := local(2026, 11, 26, 14, 32, 5)
	r := timedRenderer(timeAll, local(2026, 11, 26, 18, 0, 0))
	blocks := timedBlocks(base)
	lines := r.Lines(blocks, len(blocks), 60)

	var stamped []string
	for _, l := range lines {
		plain := ansi.Strip(l)
		trimmed := strings.TrimRight(plain, " ")
		if strings.HasSuffix(trimmed, ":05") || strings.HasSuffix(trimmed, ":25") {
			stamped = append(stamped, plain)
		}
	}
	ck.Len(stamped, 3, "stamped rows in:\n%s", ansi.Strip(strings.Join(lines, "\n")))
	for _, row := range stamped {
		ck.Eq(60, ansi.StringWidth(row), "a stamped row is not right-aligned to the width: %q", row)
	}
}

// An older block's stamp uses the empty right-hand side for its date and age.
func TestAllModeStampOfAnOldBlockCarriesDateAndAge(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := timedRenderer(timeAll, local(2026, 11, 26, 18, 0, 0))
	r.width = 80
	at := local(2026, 11, 20, 14, 32, 5)
	now := local(2026, 11, 26, 18, 0, 0)

	full := ansi.Strip(r.stampRow("▌ hi", at, now))
	ck.True(strings.HasSuffix(full, "Nov 20 14:32:05 · 6d ago"), "full form: %q", full)
	ck.Eq(80, ansi.StringWidth(full), "right-aligned")

	// Too narrow for the age: drop it, keep the date.
	r.width = 28
	mid := ansi.Strip(r.stampRow("▌ hi", at, now))
	ck.True(strings.HasSuffix(mid, "Nov 20 14:32:05"), "no age form: %q", mid)
	ck.NotStrContains(mid, "ago", "the age survived a narrow row:\n")

	// Too narrow for the date: the bare clock.
	r.width = 16
	short := ansi.Strip(r.stampRow("▌ hi", at, now))
	ck.True(strings.HasSuffix(short, "14:32:05"), "clock only form: %q", short)
	ck.NotStrContains(short, "Nov", "the date survived a narrow row:\n")

	// A block from today never grows a date or an age.
	r.width = 80
	today := ansi.Strip(r.stampRow("▌ hi", local(2026, 11, 26, 9, 0, 0), now))
	ck.True(strings.HasSuffix(today, "09:00:00"), "today: %q", today)
	ck.NotStrContains(today, "ago", "a block from today carries an age:\n")
}

// A first row with no room keeps its content; the stamp is dropped, not
// wrapped over it.
func TestStampIsDroppedWhenTheRowIsFull(t *testing.T) {
	r := newRenderer()
	r.width = 20
	row := strings.Repeat("x", 19)
	now := local(2026, 11, 26, 18, 0, 0)
	assert.NewCollecting(t).Eq(row, r.stampRow(row, local(2026, 11, 26, 14, 32, 5), now), "row")
}

// Blocks with no time (a pending send's ghost) draw no divider.
func TestBlockWithoutATimeDrawsNoDivider(t *testing.T) {
	r := timedRenderer(timeAll, local(2026, 11, 26, 18, 0, 0))
	blocks := []session.Block{{Kind: session.KindPendingUser, Text: "queued", Final: true}}
	out := ansi.Strip(strings.Join(r.Lines(blocks, 1, 60), "\n"))
	assert.NewCollecting(t).NotStrContains(out, "──", "an untimed block drew a divider:\n")
}

// ^T through the real key path: the cache holds finalized blocks, so the
// toggle only shows if the panes are invalidated, not just re-signed.
func TestCtrlTCyclesTimingAndRedrawsCachedBlocks(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.focusChild = "c_1"
	then := earlierToday()
	ev := userMessageEventFor("c_1", "hello there", 0)
	ev.Ts = timestamppb.New(then)
	c.applyEvent(ev)
	stamp := then.Local().Format("15:04:05")
	divider := "── " + then.Local().Format("15:04") + " ──"

	gaps := ansi.Strip(c.View().Content)
	ck.StrContains(gaps, divider, "the default mode drew no divider:\n")
	ck.NotStrContains(gaps, stamp, "the default mode stamped the message:\n")

	c.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	ck.Eq(timeAll, c.timeMode, "first ^T")
	all := ansi.Strip(c.View().Content)
	ck.StrContains(all, stamp, "^T did not stamp the already-cached message:\n")
	ck.NotStrContains(all, divider, "every-message mode kept the divider beside the stamp:\n")

	c.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	ck.Eq(timeOff, c.timeMode, "second ^T")
	off := ansi.Strip(c.View().Content)
	ck.NotStrContains(off, divider, "off mode kept the divider:\n")
	ck.NotStrContains(off, stamp, "off mode kept the stamp:\n")

	c.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	ck.Eq(timePeriodic, c.timeMode, "third ^T wraps")
}

func TestHelpAndFooterNameTheTimeKey(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 200, Height: 40})

	h := c.keys.ToggleTime.Help()
	ck.StrContains(ansi.Strip(c.footerHints()), h.Key+" "+h.Desc, "the footer does not name the time key:\n")

	c.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	ck.StrContains(ansi.Strip(c.View().Content), "Cycle the timing", "the help page does not describe the time key:\n")
}

// paneState.linesFor must drop the renderer's cache when only the mode changed
// (no handler invalidating for it): a finalized block cached without its
// divider is wrong in the new mode, not stale.
func TestPaneRebuildsCachedBlocksWhenOnlyTheTimeModeChanges(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	s := session.New("c_1")
	then := earlierToday()
	ev := userMessageEventFor("c_1", "hello", 0)
	ev.Ts = timestamppb.New(then)
	s.ApplyHistory(ev)
	ck.Require().Eq(1, s.Finalized, "setup: the block must be finalized so it is cached")

	p := c.pane("c_1")
	join := func(lines []string) string { return ansi.Strip(strings.Join(lines, "\n")) }
	off := p.linesFor(s, 80, 20, renderOpts{timeMode: timeOff}, overlay{})
	ck.NotStrContains(join(off), "──", "off drew a divider:\n")

	gaps := p.linesFor(s, 80, 20, renderOpts{timeMode: timePeriodic}, overlay{})
	ck.StrContains(join(gaps), "── "+then.Local().Format("15:04")+" ──",
		"the cached block was not redrawn when the mode changed:\n")
}

// earlierToday is a fixed clock time on the current day, so a test that runs
// against the real clock never lands on the other side of midnight from it.
func earlierToday() time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 3, 4, 5, 0, time.Local)
}

// A user block opens with a blank spacer row. The stamp belongs on the row
// with the message, never alone on the spacer above it.
func TestAllModeStampSharesTheMessagesOwnRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := timedRenderer(timeAll, local(2026, 11, 26, 18, 0, 0))
	blocks := []session.Block{
		{Kind: session.KindUser, At: local(2026, 11, 25, 14, 28, 56), Text: "do we need the filter", Final: true},
		{Kind: session.KindAssistant, At: local(2026, 11, 25, 14, 29, 2), Text: "No. I'd drop it.", Final: true},
	}
	lines := r.Lines(blocks, len(blocks), 100)

	stamped := 0
	for _, l := range lines {
		plain := ansi.Strip(l)
		if !strings.Contains(plain, "ago") {
			continue
		}
		stamped++
		before, _, _ := strings.Cut(plain, "Nov 25")
		ck.NotEq("", strings.TrimSpace(before), "a stamp sits alone on its row: %q", plain)
	}
	ck.Eq(2, stamped, "stamped rows in:\n%s", ansi.Strip(strings.Join(lines, "\n")))
}

func TestFirstContentRowSkipsBlankSpacers(t *testing.T) {
	ck := assert.NewCollecting(t)
	ck.Eq(2, firstContentRow([]string{"", "   ", "x", "y"}), "after spacers")
	ck.Eq(0, firstContentRow([]string{"x"}), "no spacer")
	ck.Eq(-1, firstContentRow([]string{"", " "}), "nothing drawn")
	ck.Eq(-1, firstContentRow(nil), "empty")
}

// Every message is stamped, so a divider would say the same thing twice.
func TestAllModeDrawsNoDividers(t *testing.T) {
	ck := assert.NewCollecting(t)
	out := renderTimed(timeAll, local(2026, 11, 25, 14, 32, 5), local(2026, 11, 26, 18, 0, 0))
	ck.Len(dividerRow.FindAllString(out, -1), 0, "every-message mode drew a divider:\n%s", out)
	ck.StrContains(out, "Nov 25 14:32:05 · 1d ago", "the message is not stamped:\n")
	ck.StrContains(out, "12.4s", "the turn duration was dropped:\n")
}

func TestPeriodicDividersAcrossFourMinuteWaits(t *testing.T) {
	base := local(2026, 11, 26, 10, 0, 0)
	var blocks []session.Block
	for i := range 5 {
		blocks = append(blocks, session.Block{Kind: session.KindInjected, At: base.Add(time.Duration(i) * 4 * time.Minute), Text: "waiting"})
	}
	r := timedRenderer(timePeriodic, base)
	for _, finalized := range []int{0, 1, 2, 3, 4, 5, 0} {
		out := ansi.Strip(strings.Join(r.Lines(blocks, finalized, 80), "\n"))
		assert.NewCollecting(t).EqDeep([]string{"── 10:00 ──", "── 10:08 ──", "── 10:16 ──"}, trimAll(dividerRow.FindAllString(out, -1)), "finalized=%d", finalized)
	}
}
