// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// timeMode is how much of the transcript's timing is drawn. ^T cycles it. The
// zero value is the default, so a renderer built without setting it draws gap
// dividers rather than nothing.
type timeMode int

const (
	// timeGaps draws a dim divider where the conversation paused, where the
	// day changed, and above the first block, plus each finished turn's
	// duration.
	timeGaps timeMode = iota
	// timeAll stamps every block's first row with its time (and date and age
	// when it is not from today) INSTEAD of drawing dividers, which it would
	// only repeat. Turn durations stay.
	timeAll
	// timeOff draws no timing beyond a tool call's own duration.
	timeOff

	timeModes = int(timeOff) + 1
)

func (m timeMode) String() string {
	switch m {
	case timeGaps:
		return "gaps"
	case timeAll:
		return "every message"
	case timeOff:
		return "off"
	}
	return "?"
}

// timeGapThreshold is how long the conversation must be quiet before a
// divider marks the pause. Shorter pauses are the ordinary rhythm of a turn.
const timeGapThreshold = 5 * time.Minute

// firstContentRow is the index of the first row that draws anything, or -1.
func firstContentRow(lines []string) int {
	for i, l := range lines {
		if strings.TrimSpace(ansi.Strip(l)) != "" {
			return i
		}
	}
	return -1
}

// now is the renderer's clock. Tests replace it so a fixed transcript renders
// the same on every day.
func (r *renderer) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// drawBlock renders blocks[i] with its timing decoration.
//
// The decoration depends only on blocks[i] and blocks[i-1], whose At never
// changes, so a block's lines stay valid in the renderer's cache for as long
// as the mode does (paneSig carries the mode and linesFor drops the cache when
// it changes).
func (r *renderer) drawBlock(blocks []session.Block, i int) []string {
	lines := blockLines(r.renderBlock(blocks[i]))
	b := blocks[i]
	if r.timeMode == timeOff || b.At.IsZero() {
		return lines
	}
	if r.timeMode == timeAll {
		// Every message carries its own time, date and age, so a divider would
		// say the same thing twice. The stamp goes on the first row WITH
		// content: a user block opens with a blank spacer, and a stamp on that
		// row floats alone above the message it belongs to.
		if row := firstContentRow(lines); row >= 0 {
			lines[row] = r.stampRow(lines[row], b.At, r.now())
		}
		return lines
	}
	var prev time.Time
	if i > 0 {
		prev = blocks[i-1].At
	}
	if label, ok := dividerLabel(b.At, prev, r.now()); ok {
		return append([]string{r.dividerRow(label)}, lines...)
	}
	return lines
}

// dividerLabel says whether a divider belongs above a block at `at` that
// follows a block at `prev` (zero when there is none, or it carries no time),
// and what it reads.
//
// A divider describes itself: anything not from today carries its date and how
// long ago that was, because a reader opening an old conversation has no other
// clue what day they are looking at. A block from today reads as a bare time,
// unless the day just changed.
func dividerLabel(at, prev, now time.Time) (string, bool) {
	at, prev, now = at.Local(), prev.Local(), now.Local()
	havePrev := !prev.IsZero() && prev.Unix() > 0
	if havePrev && at.Sub(prev) <= timeGapThreshold && sameDay(at, prev) {
		return "", false
	}
	if sameDay(at, now) && (!havePrev || sameDay(at, prev)) {
		return at.Format("15:04"), true
	}
	label := dateLabel(at, now, true) + " " + at.Format("15:04")
	if age := ageLabel(at, now); age != "" {
		label += " · " + age
	}
	return label, true
}

// dateLabel is "Thu Nov 20", or "Nov 20" without the weekday, with the year
// added when it is not the current one.
func dateLabel(at, now time.Time, weekday bool) string {
	layout := "Jan 2"
	if weekday {
		layout = "Mon Jan 2"
	}
	if at.Year() != now.Year() {
		layout += " 2006"
	}
	return at.Format(layout)
}

// ageLabel is how long ago `at` was, in calendar days and coarser: "1d ago",
// "2w ago", "5mo ago", "1y ago". Empty for today. It stops at days on purpose:
// finer ages go stale in the renderer's cache, a day count only changes at
// midnight.
func ageLabel(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	ay, am, ad := at.Date()
	ny, nm, nd := now.Date()
	days := int(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days < 1:
		return ""
	case days < 14:
		return itoa(int64(days)) + "d ago"
	case days < 60:
		return itoa(int64(days/7)) + "w ago"
	case days < 365:
		return itoa(int64(days/30)) + "mo ago"
	}
	return itoa(int64(days/365)) + "y ago"
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// dividerRow centres "── label ──" in the transcript width.
func (r *renderer) dividerRow(label string) string {
	text := "── " + label + " ──"
	pad := max((r.width-ansi.StringWidth(text))/2, 0)
	return strings.Repeat(" ", pad) + styleMeta.Render(text)
}

// stampRow right-aligns the block's time on its first row. A block from today
// reads "14:32:05"; an older one adds its date and age, "Nov 20 14:32:05 · 6d
// ago", since the right-hand side of a message row is otherwise empty. The
// fullest form that fits is used, and a row with no room keeps its content:
// a long first row is worth more than its timestamp.
func (r *renderer) stampRow(line string, at, now time.Time) string {
	at = at.Local()
	clock := at.Format("15:04:05")
	forms := []string{clock}
	if !sameDay(at, now.Local()) {
		dated := dateLabel(at, now.Local(), false) + " " + clock
		forms = []string{dated, clock}
		if age := ageLabel(at, now); age != "" {
			forms = []string{dated + " · " + age, dated, clock}
		}
	}
	used := ansi.StringWidth(line)
	for _, f := range forms {
		if gap := r.width - used - ansi.StringWidth(f); gap >= 2 {
			return line + strings.Repeat(" ", gap) + styleMeta.Render(f)
		}
	}
	return line
}

// turnDurStr formats a turn's length: 840ms, 12.4s, 2m05s, 1h04m.
func turnDurStr(d time.Duration) string {
	switch {
	case d < time.Minute:
		return durStr(d.Milliseconds())
	case d < time.Hour:
		return itoa(int64(d/time.Minute)) + "m" + pad2(int64(d%time.Minute/time.Second)) + "s"
	}
	return itoa(int64(d/time.Hour)) + "h" + pad2(int64(d%time.Hour/time.Minute)) + "m"
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
