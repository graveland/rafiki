package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/store"
)

// viewEntries renders the frames for msgs and pairs them with their origins —
// the exact route a db-sourced agent_view takes.
func viewEntries(t *testing.T, msgs []store.Message) []entry {
	t.Helper()
	frames, origins := fundi.DBToPiFramesWithOrigins(msgs)
	return renderEntries(frames, origins)
}

// viewIndexOf resolves a cursor in a test, failing loudly when it does not.
func viewIndexOf(t *testing.T, all []entry, cursor string) int {
	t.Helper()
	i, err := indexOfCursor(all, cursor)
	if err != nil {
		t.Fatalf("indexOfCursor(%q): %v", cursor, err)
	}
	return i
}

// viewPage parses a rendered page into its data lines (whole pageLine values,
// cursors included) and the cursors its markers name.
func viewPage(out string) (lines []string, before, after string, hasBefore, hasAfter bool) {
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "…["):
			if i := strings.Index(line, "before="); i >= 0 {
				before = strings.TrimSuffix(line[i+len("before="):], "]")
				hasBefore = true
			}
			if i := strings.Index(line, "after="); i >= 0 {
				after = strings.TrimSuffix(line[i+len("after="):], "]")
				hasAfter = true
			}
		case strings.HasPrefix(line, "["):
			// The header, or an empty window's bounds line.
		default:
			lines = append(lines, line)
		}
	}
	return lines, before, after, hasBefore, hasAfter
}

// viewEntryChunk splits one renderEntry output into its text and the offset
// that continues it, dropping the header and the trailing marker.
func viewEntryChunk(t *testing.T, out string) (text string, next int, more bool) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("renderEntry returned nothing")
	}
	body := lines[1:]
	if n := len(body); n > 0 {
		last := body[n-1]
		switch {
		case strings.HasPrefix(last, "…[more — entry="):
			i := strings.LastIndex(last, "@")
			if i < 0 {
				t.Fatalf("continuation marker without an offset: %q", last)
			}
			off, err := strconv.Atoi(strings.TrimSuffix(last[i+1:], "]"))
			if err != nil {
				t.Fatalf("continuation marker offset: %v (%q)", err, last)
			}
			next, more = off, true
			body = body[:n-1]
		case last == endedEarlyMarker:
			body = body[:n-1]
		}
	}
	return strings.Join(body, "\n"), next, more
}

func viewCursors(all []entry) []string {
	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.cursor)
	}
	return out
}

func viewDataLines(t *testing.T, out string) []string {
	t.Helper()
	lines, _, _, _, _ := viewPage(out)
	return lines
}

// TestTranscriptViewNumbersEntriesWithinARow pins D1: one row can render as
// several entries, so the cursor carries the entry's index within its row —
// and the index resets when the row changes.
func TestTranscriptViewNumbersEntriesWithinARow(t *testing.T) {
	ck := assert.NewCollecting(t)
	all := viewEntries(t, []store.Message{
		{Ordinal: 2, Param: anthropic.NewUserMessage(
			anthropic.NewToolResultBlock("tu_1", "first result", false),
			anthropic.NewToolResultBlock("tu_2", "second result", false),
			anthropic.NewToolResultBlock("tu_3", "third result", true),
		)},
		{Ordinal: 3, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("next prompt"))},
	})
	ck.EqDeep([]string{"m:2.0", "m:2.1", "m:2.2", "m:3.0"}, viewCursors(all),
		"three tool results in row 2 are m:2.0..m:2.2, and row 3 restarts at sub 0")
	ck.StrContains(all[2].page, "[ERROR]", "the third result was an error")
}

// TestTranscriptViewThinkingOnlyRowYieldsNoEntry pins that a row rendering
// nothing gets no cursor — there is nothing to address.
func TestTranscriptViewThinkingOnlyRowYieldsNoEntry(t *testing.T) {
	ck := assert.NewCollecting(t)
	all := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("prompt"))},
		{Ordinal: 1, StopReason: string(anthropic.StopReasonEndTurn), Param: anthropic.NewAssistantMessage(
			anthropic.NewThinkingBlock("sig", "long private reasoning"))},
	})
	ck.EqDeep([]string{"m:0.0"}, viewCursors(all), "a thinking-only row renders no entry")
	for _, e := range all {
		ck.NotStrContains(e.full, "long private reasoning", "thinking never reaches the reader")
	}
}

// TestTranscriptViewEndedEarly pins D8: a row the MODEL did not finish is
// marked, including one that ran out of tokens mid-thinking and so has no
// text to show.
func TestTranscriptViewEndedEarly(t *testing.T) {
	ck := assert.NewCollecting(t)
	all := viewEntries(t, []store.Message{
		{Ordinal: 0, StopReason: string(anthropic.StopReasonMaxTokens), Param: anthropic.NewAssistantMessage(
			anthropic.NewThinkingBlock("sig", "ran out"))},
		{Ordinal: 1, StopReason: string(anthropic.StopReasonMaxTokens), Param: anthropic.NewAssistantMessage(
			anthropic.NewTextBlock("cut short"))},
		{Ordinal: 2, StopReason: string(anthropic.StopReasonEndTurn), Param: anthropic.NewAssistantMessage(
			anthropic.NewTextBlock("finished"))},
	})
	ck.EqDeep([]string{"m:0.0", "m:1.0", "m:2.0"}, viewCursors(all),
		"a thinking-only row that ended early still yields an entry")
	ck.Eq("assistant: (no text)", all[0].page, "the marker needs an entry to attach to")
	ck.Eq(true, all[0].endsEarly, "max_tokens is the marked stop reason")
	ck.True(strings.HasSuffix(pageLine(all[0]), " ⟪ended early: max_tokens⟫"),
		"pageLine must carry the marker: %q", pageLine(all[0]))
	ck.True(strings.HasSuffix(pageLine(all[1]), " ⟪ended early: max_tokens⟫"),
		"a row with text that ended early is marked too: %q", pageLine(all[1]))
	ck.NotStrContains(pageLine(all[2]), "ended early", "a finished row carries no marker")
}

// TestTranscriptViewFullTextIsUncut pins the "full text" contract: entry= and
// find= see real newlines and the whole message, where the page form is
// flattened and truncated — and the page says which cursor continues it.
func TestTranscriptViewFullTextIsUncut(t *testing.T) {
	ck := assert.NewCollecting(t)
	text := strings.Repeat("line one\n", 600)
	all := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock(text))},
	})
	e := all[0]
	ck.StrContains(e.page, "…(truncated)", "the page form is capped at 2000")
	ck.StrContains(e.page, " ⏎ ", "the page form rewrites newlines")
	ck.NotStrContains(e.page, "\n", "the page form is one line")
	ck.True(strings.Contains(e.full, "\n"), "the full form keeps real newlines")
	ck.Eq(len("assistant: ")+len(strings.TrimSpace(text)), len(e.full), "the full form is not truncated")
	ck.Eq(true, e.cut, "the page lost text the full form has")
	ck.StrContains(pageLine(e), "entry=m:0.0", "the truncated line names the cursor that reads the rest")
}

// TestTranscriptViewNewestPageMarkers pins the page frame: a total, the shown
// range, and one marker per direction that has more.
func TestTranscriptViewNewestPageMarkers(t *testing.T) {
	ck := assert.NewCollecting(t)
	var msgs []store.Message
	for i := 0; i < 50; i++ {
		msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
			anthropic.NewTextBlock(fmt.Sprintf("prompt %d", i)))})
	}
	all := viewEntries(t, msgs)
	out := renderPage(all, pageSpec{mode: pageNewest, limit: 40, maxBytes: viewMaxBytes})
	ck.StrContains(out, "[50 entries · showing m:10.0..m:49.0]", "the header names the total and the range")
	ck.StrContains(out, "…[10 earlier — before=m:10.0]", "the oldest shown entry is the way further back")
	ck.NotStrContains(out, "later", "the newest page has nothing after it")
	ck.Eq(40, len(viewDataLines(t, out)), "limit 40 shows 40 entries")
}

// TestTranscriptViewByteBudgetFromEnd pins that the newest and before pages
// drop the OLDEST entries of their window when the byte budget binds.
func TestTranscriptViewByteBudgetFromEnd(t *testing.T) {
	ck := assert.NewCollecting(t)
	var msgs []store.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
			anthropic.NewTextBlock(strings.Repeat("x", 100)))})
	}
	all := viewEntries(t, msgs)
	lineLen := len(pageLine(all[0]))
	for _, e := range all {
		ck.Eq(lineLen, len(pageLine(e)), "the fixture's entries are the same size")
	}
	maxBytes := 3 * (lineLen + 1) // exactly three fit: each line costs its text plus its newline

	newest := renderPage(all, pageSpec{mode: pageNewest, limit: 0, maxBytes: maxBytes})
	ck.StrContains(newest, "showing m:3.0..m:5.0", "the byte budget keeps the NEWEST three")
	ck.StrContains(newest, "…[3 earlier — before=m:3.0]", "and says how to read back")

	before := renderPage(all, pageSpec{mode: pageBefore, at: 5, limit: 0, maxBytes: maxBytes})
	ck.StrContains(before, "showing m:2.0..m:4.0", "a before page keeps the LAST three of its window")
	ck.StrContains(before, "…[2 earlier — before=m:2.0]", "earlier marker")
	ck.StrContains(before, "…[1 later — after=m:4.0]", "later marker")
}

// TestTranscriptViewByteBudgetFromStartOnAfter pins that after pages take the
// FIRST entries of their window, so consecutive pages are contiguous. It dies
// if after reuses the from-end walk.
func TestTranscriptViewByteBudgetFromStartOnAfter(t *testing.T) {
	ck := assert.NewCollecting(t)
	var msgs []store.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
			anthropic.NewTextBlock(strings.Repeat("x", 100)))})
	}
	all := viewEntries(t, msgs)
	maxBytes := 3 * (len(pageLine(all[0])) + 1) // exactly three fit
	out := renderPage(all, pageSpec{mode: pageAfter, at: 0, limit: 0, maxBytes: maxBytes})
	ck.StrContains(out, "showing m:1.0..m:3.0", "the byte budget keeps the FIRST three after the cursor")
	ck.StrContains(out, "…[2 later — after=m:3.0]", "and names where to continue")
	ck.NotStrContains(out, "m:0.0 ", "the cursor itself is not re-shown")
	ck.NotStrContains(out, "m:4.0 ", "and nothing past the budget leaks in")
}

// TestTranscriptViewOversizeEntryStillShown pins D3's floor: a page is never
// empty while entries remain, even when one entry alone exceeds the budget.
func TestTranscriptViewOversizeEntryStillShown(t *testing.T) {
	ck := assert.NewCollecting(t)
	all := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock(strings.Repeat("y", 5000)))},
	})
	out := renderPage(all, pageSpec{mode: pageNewest, limit: 0, maxBytes: 10})
	ck.StrContains(out, pageLine(all[0]), "the oversize entry is shown alone")
	ck.NotStrContains(out, "(nothing", "an oversize entry is not an empty page")
	ck.StrContains(out, "entry=m:0.0", "and it names the cursor that reads it whole")
}

// TestTranscriptViewPagingIsLossless is the equivalence test: walking back
// from the newest page and then forward from the oldest reassembles every
// entry exactly once, in order. It dies on any off-by-one in the window maths,
// a marker naming the wrong cursor, or a page that skips or repeats an entry.
func TestTranscriptViewPagingIsLossless(t *testing.T) {
	ck := assert.NewCollecting(t)
	var msgs []store.Message
	for i := 0; i < 137; i++ {
		switch i % 4 {
		case 0:
			msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
				anthropic.NewTextBlock(fmt.Sprintf("prompt %d", i)))})
		case 1:
			msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewAssistantMessage(
				anthropic.NewTextBlock(strings.Repeat(fmt.Sprintf("answer %d ", i), 5)))})
		case 2:
			msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(fmt.Sprintf("tu_%d", i), fmt.Sprintf("result %d", i), false))})
		case 3:
			msgs = append(msgs, store.Message{Ordinal: i, Kind: ptr(store.KindCompactionSummary),
				InputTokens: ptr(1000 * i), Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary"))})
		}
	}
	all := viewEntries(t, msgs)
	ck.Eq(137, len(all), "one entry per row, dividers included")

	want := make([]string, len(all))
	for i, e := range all {
		want[i] = pageLine(e)
	}

	spec := func(mode pageMode, at int) pageSpec {
		return pageSpec{mode: mode, at: at, limit: 20, maxBytes: viewMaxBytes}
	}

	// Backward: newest page, then follow every before= marker to the start.
	var backward [][]string
	out := renderPage(all, spec(pageNewest, 0))
	for hops := 0; ; hops++ {
		if hops > len(all) {
			t.Fatalf("before= walk did not terminate after %d pages", hops)
		}
		lines, before, _, hasBefore, _ := viewPage(out)
		backward = append(backward, lines)
		if !hasBefore {
			break
		}
		out = renderPage(all, spec(pageBefore, viewIndexOf(t, all, before)))
	}
	var backFlat []string
	for i := len(backward) - 1; i >= 0; i-- {
		backFlat = append(backFlat, backward[i]...)
	}
	ck.EqDeep(want, backFlat, "paging back from the newest page reassembles the transcript")

	// Forward: from the oldest page, follow every after= marker to the end.
	oldest := renderPage(all, spec(pageNewest, 0))
	for {
		_, before, _, hasBefore, _ := viewPage(oldest)
		if !hasBefore {
			break
		}
		oldest = renderPage(all, spec(pageBefore, viewIndexOf(t, all, before)))
	}
	var forward []string
	out = oldest
	for hops := 0; ; hops++ {
		if hops > len(all) {
			t.Fatalf("after= walk did not terminate after %d pages", hops)
		}
		lines, _, after, _, hasAfter := viewPage(out)
		forward = append(forward, lines...)
		if !hasAfter {
			break
		}
		out = renderPage(all, spec(pageAfter, viewIndexOf(t, all, after)))
	}
	ck.EqDeep(want, forward, "paging forward from the oldest page reassembles the transcript")
}

// TestTranscriptViewCursorErrors pins every refusal a cursor can earn: the
// grammar, an absent entry with the real bounds, and the two empty windows.
func TestTranscriptViewCursorErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	var msgs []store.Message
	for i := 0; i < 3; i++ {
		msgs = append(msgs, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
			anthropic.NewTextBlock(fmt.Sprintf("prompt %d", i)))})
	}
	all := viewEntries(t, msgs)

	for _, bad := range []string{"x", "m:1", "m:-1.0", "m:1.0.0"} {
		_, err := indexOfCursor(all, bad)
		ck.ErrorContains(err, "malformed cursor", "%q is not a cursor", bad)
	}

	_, err := indexOfCursor(all, "m:999.0")
	ck.ErrorContains(err, "no entry m:999.0 (", "an absent cursor names itself")
	ck.StrContains(err.Error(), bounds(all), "and the real bounds: %v", err)

	before := renderPage(all, pageSpec{mode: pageBefore, at: 0, limit: 0, maxBytes: viewMaxBytes})
	ck.StrContains(before, "(nothing before m:0.0)", "the first entry has nothing before it")
	ck.StrContains(before, "["+bounds(all)+"]", "an empty page still names the bounds")

	last := len(all) - 1
	after := renderPage(all, pageSpec{mode: pageAfter, at: last, limit: 0, maxBytes: viewMaxBytes})
	ck.StrContains(after, "(nothing after "+all[last].cursor+")", "the last entry has nothing after it")
}

// TestTranscriptViewFind pins D5: find sees past both per-entry caps, matches
// case-insensitively per rune (so a non-ASCII corpus still lands on the
// match), reports the total, and hands back a cursor for the rest.
func TestTranscriptViewFind(t *testing.T) {
	ck := assert.NewCollecting(t)

	pastMessageCap := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewAssistantMessage(
			anthropic.NewTextBlock(strings.Repeat("a", 2500) + " needle"))},
	})
	out := renderFind(pastMessageCap, "needle", 0, 0, viewMaxBytes)
	ck.StrContains(out, "[1 matches for \"needle\"", "a hit past the 2000-char page cap is still found")
	ck.StrContains(out, "needle", "and the snippet shows the match")

	pastToolCap := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(
			anthropic.NewToolResultBlock("tu_1", strings.Repeat("b", 500)+" needle", false))},
	})
	out = renderFind(pastToolCap, "needle", 0, 0, viewMaxBytes)
	ck.StrContains(out, "[1 matches for \"needle\"", "a hit past the 400-char tool-result cap is still found")
	ck.StrContains(out, "m:0.0  ", "the hit is addressed by its cursor")

	caseInsensitive := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("the Needle is here"))},
	})
	ck.StrContains(renderFind(caseInsensitive, "NEEDLE", 0, 0, viewMaxBytes), "Needle",
		"matching is case-insensitive and the snippet keeps the original case")

	nonASCII := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("Ärger über den needle hier"))},
	})
	ck.StrContains(renderFind(nonASCII, "needle", 0, 0, viewMaxBytes), "needle",
		"lowering per rune keeps snippet offsets valid for non-ASCII text")

	var many []store.Message
	for i := 0; i < 15; i++ {
		many = append(many, store.Message{Ordinal: i, Param: anthropic.NewUserMessage(
			anthropic.NewTextBlock("a needle in entry " + strconv.Itoa(i)))})
	}
	all := viewEntries(t, many)
	first := renderFind(all, "needle", 0, 0, viewMaxBytes)
	ck.StrContains(first, "[15 matches for \"needle\"", "the total is always reported")
	ck.Eq(10, len(viewDataLines(t, first)), "the default limit is 10 hits")
	ck.StrContains(first, "…[5 more — find=\"needle\" after=m:9.0]", "the rest is one after= away")

	rest := renderFind(all, "needle", viewIndexOf(t, all, "m:9.0")+1, 0, viewMaxBytes)
	ck.Eq(5, len(viewDataLines(t, rest)), "following after= shows the remaining five")
	ck.NotStrContains(rest, "more — find=", "and there is no marker left to follow")
	ck.StrContains(rest, "searched 5 entries", "the searched range is named from the cursor on")

	noMatch := renderFind(all, "zzz", 0, 0, viewMaxBytes)
	ck.Eq(fmt.Sprintf("no match for \"zzz\" in %s\n", bounds(all)), noMatch, "no hit names the whole transcript")
	noMatchAfter := renderFind(all, "zzz", 10, 0, viewMaxBytes)
	ck.Eq(fmt.Sprintf("no match for \"zzz\" after %s (%s)\n", all[9].cursor, bounds(all)), noMatchAfter,
		"no hit with after= names the cursor it searched from")
	ck.StrContains(renderFind(all, "", 0, 0, viewMaxBytes), "find needs text", "an empty needle is refused")
}

// TestTranscriptViewEntryChunks pins D4: entry= hands back at most 32 KiB at a
// time, always ending on a rune boundary, and the continuation offset
// reassembles the full text exactly.
func TestTranscriptViewEntryChunks(t *testing.T) {
	ck := assert.NewCollecting(t)
	// 70 000 bytes of 2-byte runes: the 32 768-byte boundary lands mid-rune.
	big := strings.Repeat("é", 35000)
	all := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("go"))},
		{Ordinal: 1, StopReason: string(anthropic.StopReasonMaxTokens),
			Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock(big))},
	})
	i := viewIndexOf(t, all, "m:1.0")
	e := all[i]

	var got []string
	arg := e.cursor
	var chunks []string
	for hop := 0; ; hop++ {
		if hop > 10 {
			t.Fatalf("entry= chunking did not terminate")
		}
		out, err := renderEntry(all, arg, viewMaxBytes)
		if err != nil {
			t.Fatalf("renderEntry(%q): %v", arg, err)
		}
		chunks = append(chunks, out)
		text, next, more := viewEntryChunk(t, out)
		ck.True(utf8.ValidString(text), "chunk %d ends on a rune boundary", hop)
		ck.True(len(text) <= viewMaxBytes, "chunk %d is within the byte cap (%d)", hop, len(text))
		got = append(got, text)
		if !more {
			break
		}
		ck.StrContains(out, fmt.Sprintf("…[more — entry=%s@%d]", e.cursor, next),
			"the continuation names the next offset")
		arg = fmt.Sprintf("%s@%d", e.cursor, next)
	}
	ck.Eq(len(chunks), 3, "70 011 bytes needs three chunks at 32 KiB")
	ck.Eq(e.full, strings.Join(got, ""), "following the offsets reassembles the full text")
	for _, out := range chunks[:len(chunks)-1] {
		ck.NotStrContains(out, endedEarlyMarker, "the ended-early marker belongs to the last chunk only")
	}
	ck.StrContains(chunks[len(chunks)-1], endedEarlyMarker, "the last chunk carries it")

	mid := 1
	for mid < len(e.full) && utf8.RuneStart(e.full[mid]) {
		mid++
	}
	ck.True(mid < len(e.full), "the fixture has a mid-rune offset")
	_, err := renderEntry(all, fmt.Sprintf("%s@%d", e.cursor, mid), viewMaxBytes)
	ck.ErrorContains(err, "splits a character", "an offset inside a rune is refused")

	_, err = renderEntry(all, fmt.Sprintf("%s@%d", e.cursor, len(e.full)), viewMaxBytes)
	ck.ErrorContains(err, "is past the end of", "an offset at the end is past it")
	ck.StrContains(err.Error(), fmt.Sprintf("(%d bytes)", len(e.full)), "the error names the entry's length")

	_, err = renderEntry(all, fmt.Sprintf("%s@nope", e.cursor), viewMaxBytes)
	ck.ErrorContains(err, "malformed offset", "a non-numeric offset is refused")

	short := viewEntries(t, []store.Message{
		{Ordinal: 0, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("short answer"))},
	})
	out, err := renderEntry(short, short[0].cursor, viewMaxBytes)
	ck.NoError(err, "a short entry renders in one chunk")
	ck.StrContains(out, fmt.Sprintf("[entry %s · %d bytes]", short[0].cursor, len(short[0].full)),
		"the whole entry is named as one chunk")
	ck.NotStrContains(out, "more — entry=", "and there is nothing to continue")
}

// TestTranscriptViewOneShotHasNoCursors pins that the non-db one-shot path
// still renders without cursors, and that a mismatched origins slice degrades
// to that rather than panicking.
func TestTranscriptViewOneShotHasNoCursors(t *testing.T) {
	ck := assert.NewCollecting(t)
	frames, origins := fundi.DBToPiFramesWithOrigins([]store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi there"))},
	})

	for _, e := range renderEntries(frames, nil) {
		ck.Eq("", e.cursor, "no origins means no cursors")
	}
	oneShot := renderTranscript(frames, 0, viewMaxBytes)
	ck.NotStrContains(oneShot, "m:", "the one-shot output carries no cursor")
	ck.StrContains(oneShot, "user: hello", "and still renders the transcript")

	short := renderEntries(frames, origins[:len(origins)-1])
	for _, e := range short {
		ck.Eq("", e.cursor, "a mismatched origins slice falls back to no cursors")
	}
	ck.Eq(len(renderEntries(frames, nil)), len(short), "and renders the same entries")
}
