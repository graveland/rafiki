package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/msg"
)

// --- transcript rendering ---

const (
	viewDefaultEntries = 40
	viewMaxEntries     = 200
	// viewMaxBytes caps the rendered transcript. A tool result is inlined
	// into the caller's next request, so an unbounded view would let one
	// agent_view call blow the coordinator's context window — a limit the
	// frame cap does not cover, because this never crosses a frame.
	viewMaxBytes = 32 * 1024
	// findDefaultHits is the default number of hits one find= call returns;
	// an explicit limit overrides it. The total is always reported, so the
	// caller can page through the rest with after=.
	findDefaultHits = 10
	// endedEarlyMarker marks a message the MODEL did not finish, as opposed to
	// one the tool stopped reading (which always names its own cursor). Only
	// max_tokens is marked in v1.
	endedEarlyMarker = "⟪ended early: max_tokens⟫"
)

// entry is one rendered line of a transcript, in both the page form the tool
// prints and the full form entry= and find= read.
//
// cursor is "m:<ordinal>.<sub>" — the storage ordinal of the row the line came
// from plus the line's index within that row, because one row can render as
// several lines. It is empty when the entries were rendered without origins
// (the one-shot non-db path), which is also what pageLine uses to decide
// whether a truncation can name a way to continue.
type entry struct {
	cursor    string
	page      string
	full      string
	cut       bool
	endsEarly bool
}

// renderEntries walks pi-vocabulary frames the way renderTranscript always
// has, producing one entry per rendered line. It is pure: the whole paging
// surface is a slice operation over its output.
//
// origins runs parallel to events. When it is nil — or the wrong length — no
// entry carries a cursor, which is the one-shot shape.
func renderEntries(events []json.RawMessage, origins []fundi.FrameOrigin) []entry {
	if len(origins) != len(events) {
		// A mismatched origins slice is treated as absent rather than indexed
		// past: a frame and its origin are one record, and half of one is not
		// worth a panic.
		origins = nil
	}
	var out []entry
	prevOrdinal := -1
	sub := 0
	for i, raw := range events {
		var env transcriptFrame
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		var e entry
		switch env.Type {
		case "message_end":
			page, full, cut, ok := parseMessageContent(env.Message.Role, env.Message.Content)
			if !ok {
				// A row that renders no text still yields an entry when the
				// model ran out of tokens mid-message: without one, the marker
				// would have nowhere to attach and the cut would read as "the
				// tool stopped".
				if origins == nil || origins[i].StopReason != msg.StopMaxTokens.String() {
					continue
				}
				page = env.Message.Role + ": (no text)"
				full = page
			}
			e = entry{page: page, full: full, cut: cut}
		case "compaction_boundary":
			// The divider pkg/fundi.DBToPiFramesMessages emits for a stored
			// compaction_summary row, mirroring the TUI's boundary block.
			var line string
			if env.Trigger == "clear" {
				line = "— context cleared —"
			} else if env.PreTokens != nil {
				line = fmt.Sprintf("— context compacted · ~%dk tokens —", *env.PreTokens/1000)
			} else {
				line = "— context compacted —"
			}
			e = entry{page: line, full: line}
		case "tool_execution_end":
			text := ""
			if len(env.Result.Content) > 0 {
				text = env.Result.Content[0].Text
			}
			status := "ok"
			if env.IsError {
				status = "ERROR"
			}
			e = entry{
				page: fmt.Sprintf("  → %s [%s] %s", env.ToolName, status, truncateLine(text, 400)),
				full: fmt.Sprintf("→ %s [%s]\n%s", env.ToolName, status, strings.TrimSpace(text)),
				cut:  len(flattenText(text)) > 400,
			}
		default:
			// message_start, message_update, tool_execution_start, agent_end,
			// …: render nothing, so there is nothing to name.
			continue
		}
		if origins != nil {
			o := origins[i]
			if o.FromRow {
				if o.Ordinal == prevOrdinal {
					sub++
				} else {
					prevOrdinal = o.Ordinal
					sub = 0
				}
				e.cursor = fmt.Sprintf("m:%d.%d", o.Ordinal, sub)
			}
			e.endsEarly = o.FromRow && o.StopReason == msg.StopMaxTokens.String()
		}
		out = append(out, e)
	}
	return out
}

// transcriptFrame is one pi-vocabulary event, decoded for the fields the
// renderer reads.
type transcriptFrame struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolName  string `json:"toolName"`
	PreTokens *int   `json:"preTokens"`
	Trigger   string `json:"trigger"`
	Result    struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	IsError bool `json:"isError"`
}

// parseMessageContent renders a message_end frame's content twice from ONE
// parse: the page form (truncated, newlines rewritten) and the full form
// (uncut, real newlines, no thinking blocks). ok is false when the message
// renders nothing at all.
func parseMessageContent(role string, content json.RawMessage) (page, full string, cut, ok bool) {
	// A user message's content is a bare string (child.PiUserMessage.Content
	// is `any` and carries a string); an assistant message's is a block array.
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return "user: " + truncateLine(text, 2000),
			"user: " + strings.TrimSpace(text),
			len(flattenText(text)) > 2000,
			true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", "", false, false
	}
	var pageParts, fullParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			if len(flattenText(b.Text)) > 2000 {
				cut = true
			}
			pageParts = append(pageParts, truncateLine(b.Text, 2000))
			fullParts = append(fullParts, strings.TrimSpace(b.Text))
		case "toolCall":
			pageParts = append(pageParts, "<calls "+b.Name+">")
			fullParts = append(fullParts, "<calls "+b.Name+">")
		}
	}
	if len(pageParts) == 0 {
		return "", "", false, false
	}
	return role + ": " + strings.Join(pageParts, " "),
		role + ": " + strings.Join(fullParts, "\n\n"),
		cut,
		true
}

// renderTranscript flattens pi-vocabulary event frames into plain text a model
// can read — the one-shot form, without cursors, for the sources that cannot
// be paged yet. It deliberately renders only what a reader needs to judge what
// an agent did — prompts, assistant text, tool calls and truncated tool
// results — and drops thinking blocks, which are long and rarely load-bearing
// to an observer.
//
// Newest content is what matters, so both budgets are applied from the END:
// maxEntries keeps the newest rendered entries and maxBytes then drops the
// oldest of those, never the most recent. maxEntries counts rendered lines,
// not frames — most frames (message_start, agent_end, …) render nothing, so a
// frame limit would hand back fewer entries than asked for, or none.
//
// The legacy budget has NO never-empty rule, unlike renderPage: when even the
// newest entry exceeds maxBytes the answer is only the omitted-count line.
func renderTranscript(events []json.RawMessage, maxEntries, maxBytes int) string {
	all := renderEntries(events, nil)
	floor := 0
	if maxEntries > 0 && len(all) > maxEntries {
		floor = len(all) - maxEntries
	}
	total := 0
	cut := len(all)
	for i := len(all) - 1; i >= floor; i-- {
		if total+len(all[i].page)+1 > maxBytes {
			break
		}
		total += len(all[i].page) + 1
		cut = i
	}
	var sb strings.Builder
	if cut > 0 {
		fmt.Fprintf(&sb, "[%d earlier entries omitted]\n", cut)
	}
	for _, e := range all[cut:] {
		sb.WriteString(e.page)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "(no transcript yet)\n"
	}
	return sb.String()
}

// pageLine is how one entry appears on a page: its cursor (when it has one),
// the page text, and — after it — every reason the line is not the whole
// story, each naming how to read on.
func pageLine(e entry) string {
	var sb strings.Builder
	if e.cursor != "" {
		sb.WriteString(e.cursor)
		sb.WriteString(" ")
	}
	sb.WriteString(e.page)
	if e.endsEarly {
		sb.WriteString(" ")
		sb.WriteString(endedEarlyMarker)
	}
	if e.cut && e.cursor != "" {
		sb.WriteString(" …[entry truncated — entry=")
		sb.WriteString(e.cursor)
		sb.WriteString("]")
	}
	return sb.String()
}

// flattenText is the form truncateLine measures and prints: trimmed, with
// newlines rewritten so a line stays one line.
func flattenText(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
}

func truncateLine(s string, max int) string {
	s = flattenText(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// --- cursors ---

// parseCursor accepts exactly m:<ordinal>.<sub> (decimal, non-negative).
func parseCursor(s string) (ordinal, sub int, err error) {
	bad := fmt.Errorf("malformed cursor %q (want m:<ordinal>.<n>, as a marker printed it)", s)
	rest, ok := strings.CutPrefix(s, "m:")
	if !ok {
		return 0, 0, bad
	}
	ordStr, subStr, ok := strings.Cut(rest, ".")
	if !ok || !allDigits(ordStr) || !allDigits(subStr) {
		return 0, 0, bad
	}
	ordinal, err = strconv.Atoi(ordStr)
	if err != nil {
		return 0, 0, bad
	}
	sub, err = strconv.Atoi(subStr)
	if err != nil {
		return 0, 0, bad
	}
	return ordinal, sub, nil
}

// allDigits rejects "", "+1" and "-1" before Atoi can accept them.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// indexOfCursor resolves a cursor a marker printed back to its entry.
func indexOfCursor(all []entry, cursor string) (int, error) {
	if _, _, err := parseCursor(cursor); err != nil {
		return 0, err
	}
	for i := range all {
		if all[i].cursor == cursor {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no entry %s (%s)", cursor, bounds(all))
}

// bounds names the whole transcript's extent, so a refused cursor can say what
// would have been valid.
func bounds(all []entry) string {
	if len(all) == 0 {
		return "0 entries"
	}
	return fmt.Sprintf("%d entries · %s..%s", len(all), all[0].cursor, all[len(all)-1].cursor)
}

// --- pages ---

type pageMode int

const (
	// pageNewest starts at 1: a forgotten mode is a bug, not "newest".
	pageNewest pageMode = iota + 1
	pageBefore
	pageAfter
)

type pageSpec struct {
	mode     pageMode
	at       int // index into all for before/after
	limit    int // entries; 0 = no entry cap
	maxBytes int
}

// renderPage slices all into one page and says where it stands and how to
// move. The byte budget runs from the END for newest/before pages and from
// the START for after pages, so consecutive pages are contiguous.
//
// A page is never empty while entries remain: one entry larger than the whole
// budget is shown alone, with its own entry= cursor, rather than dropped.
func renderPage(all []entry, spec pageSpec) string {
	n := len(all)
	if n == 0 {
		return "(no transcript yet)\n"
	}
	at := spec.at
	if at < 0 {
		at = 0
	}
	if at >= n {
		at = n - 1
	}
	var start, end int
	switch spec.mode {
	case pageAfter:
		start = at + 1
		end = n
		if spec.limit > 0 && start+spec.limit < n {
			end = start + spec.limit
		}
		if start < end {
			total, newEnd := 0, start
			for i := start; i < end; i++ {
				size := len(pageLine(all[i])) + 1
				if total+size > spec.maxBytes {
					break
				}
				total += size
				newEnd = i + 1
			}
			if newEnd == start {
				newEnd = start + 1
			}
			end = newEnd
		}
	case pageNewest, pageBefore:
		end = n
		if spec.mode == pageBefore {
			end = at
		}
		start = 0
		if spec.limit > 0 && end-spec.limit > 0 {
			start = end - spec.limit
		}
		if end > start {
			total, newStart := 0, end
			for i := end - 1; i >= start; i-- {
				size := len(pageLine(all[i])) + 1
				if total+size > spec.maxBytes {
					break
				}
				total += size
				newStart = i
			}
			if newStart == end {
				newStart = end - 1
			}
			start = newStart
		}
	default:
		return fmt.Sprintf("unknown page mode %d\n", spec.mode)
	}

	var sb strings.Builder
	if start >= end {
		sb.WriteString("[")
		sb.WriteString(bounds(all))
		sb.WriteString("]\n")
		if spec.mode == pageAfter {
			fmt.Fprintf(&sb, "(nothing after %s)\n", all[at].cursor)
		} else {
			fmt.Fprintf(&sb, "(nothing before %s)\n", all[at].cursor)
		}
		return sb.String()
	}
	fmt.Fprintf(&sb, "[%d entries · showing %s..%s]\n", n, all[start].cursor, all[end-1].cursor)
	if start > 0 {
		fmt.Fprintf(&sb, "…[%d earlier — before=%s]\n", start, all[start].cursor)
	}
	for _, e := range all[start:end] {
		sb.WriteString(pageLine(e))
		sb.WriteString("\n")
	}
	if end < n {
		fmt.Fprintf(&sb, "…[%d later — after=%s]\n", n-end, all[end-1].cursor)
	}
	return sb.String()
}

// --- find ---

// renderFind seeks by content over the FULL, untruncated text of every entry
// from start on. Matching lowers per rune, so the match index — and therefore
// the snippet — stays valid for non-ASCII text.
func renderFind(all []entry, needle string, start, limit, maxBytes int) string {
	if needle == "" {
		return "find needs text\n"
	}
	if start < 0 {
		start = 0
	}
	if start > len(all) {
		start = len(all)
	}
	if limit <= 0 {
		limit = findDefaultHits
	}
	folded := []rune(strings.ToLower(needle))
	type hit struct{ idx, at int }
	var hits []hit
	for i := start; i < len(all); i++ {
		r := []rune(all[i].full)
		lowered := make([]rune, len(r))
		for j, c := range r {
			lowered[j] = unicode.ToLower(c)
		}
		if k := indexRunes(lowered, folded); k >= 0 {
			hits = append(hits, hit{idx: i, at: k})
		}
	}
	var sb strings.Builder
	if len(hits) == 0 {
		if start == 0 {
			fmt.Fprintf(&sb, "no match for %q in %s\n", needle, bounds(all))
		} else {
			fmt.Fprintf(&sb, "no match for %q after %s (%s)\n", needle, all[start-1].cursor, bounds(all))
		}
		return sb.String()
	}
	fmt.Fprintf(&sb, "[%d matches for %q · searched %s]\n", len(hits), needle, bounds(all[start:]))
	shown := 0
	lastCursor := ""
	used := sb.Len()
	for _, h := range hits {
		if shown >= limit {
			break
		}
		line := all[h.idx].cursor + "  " + findSnippet([]rune(all[h.idx].full), h.at, len(folded))
		// The first hit is shown whatever its size: a header plus a
		// continuation marker that names no hit would be a dead end.
		if shown > 0 && used+len(line)+1 > maxBytes {
			break
		}
		used += len(line) + 1
		sb.WriteString(line)
		sb.WriteString("\n")
		shown++
		lastCursor = all[h.idx].cursor
	}
	if remaining := len(hits) - shown; remaining > 0 {
		fmt.Fprintf(&sb, "…[%d more — find=%q after=%s]\n", remaining, needle, lastCursor)
	}
	return sb.String()
}

// indexRunes returns the first index at which needle occurs in hay, or -1.
func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(hay) {
		return -1
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// findSnippet shows ±150 runes around a match at k in r, with the ellipses
// saying which side was cut.
func findSnippet(r []rune, k, needleLen int) string {
	from := k - 150
	if from < 0 {
		from = 0
	}
	to := k + needleLen + 150
	if to > len(r) {
		to = len(r)
	}
	s := strings.ReplaceAll(string(r[from:to]), "\n", " ⏎ ")
	if from > 0 {
		s = "…" + s
	}
	if to < len(r) {
		s += "…"
	}
	return s
}

// --- one entry ---

// renderEntry returns one entry's full text, at most maxBytes per call, and
// names the offset that continues it. arg is <cursor> or <cursor>@<offset>,
// the offset a byte position that always lands on a rune boundary.
func renderEntry(all []entry, arg string, maxBytes int) (string, error) {
	cursor := arg
	offset := 0
	if i := strings.LastIndex(arg, "@"); i >= 0 {
		cursor = arg[:i]
		raw := arg[i+1:]
		if !allDigits(raw) {
			return "", fmt.Errorf("malformed offset %q", raw)
		}
		off, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("malformed offset %q", raw)
		}
		offset = off
	}
	idx, err := indexOfCursor(all, cursor)
	if err != nil {
		return "", err
	}
	e := all[idx]
	text := e.full
	if offset > len(text) || (offset == len(text) && len(text) > 0) {
		return "", fmt.Errorf("offset %d is past the end of %s (%d bytes)", offset, e.cursor, len(text))
	}
	if offset < len(text) && !utf8.RuneStart(text[offset]) {
		return "", fmt.Errorf("offset %d splits a character in %s", offset, e.cursor)
	}
	end := len(text)
	if maxBytes > 0 && offset+maxBytes < end {
		end = offset + maxBytes
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}

	var sb strings.Builder
	if offset == 0 && end == len(text) {
		fmt.Fprintf(&sb, "[entry %s · %d bytes]\n", e.cursor, len(text))
	} else {
		fmt.Fprintf(&sb, "[entry %s · bytes %d..%d of %d]\n", e.cursor, offset, end, len(text))
	}
	sb.WriteString(text[offset:end])
	sb.WriteString("\n")
	if end == len(text) && e.endsEarly {
		sb.WriteString(endedEarlyMarker)
		sb.WriteString("\n")
	}
	if end < len(text) {
		fmt.Fprintf(&sb, "…[more — entry=%s@%d]\n", e.cursor, end)
	}
	return sb.String(), nil
}
