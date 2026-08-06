package tools

import (
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// normalizeToLF converts CRLF and solitary CR to LF.
func normalizeToLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// stripBomRunes returns s without a leading UTF-8 BOM.
func stripBomRunes(s string) string {
	if strings.HasPrefix(s, "\uFEFF") {
		return s[len("\uFEFF"):]
	}
	return s
}

// normalizeForFuzzyMatch applies progressive normalization to make LLM-emitted
// old_string more likely to match the on-disk content:
//
//   - Unicode NFKC (compatibility decomposition + composition)
//   - Strip trailing whitespace from each line
//   - Smart quotes → ASCII
//   - Unicode dashes/hyphens → ASCII -
//   - Special Unicode spaces → ASCII space
func normalizeForFuzzyMatch(s string) string {
	s = norm.NFKC.String(s)

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	s = strings.Join(lines, "\n")

	// Smart single quotes → '
	s = strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201A", "'", "\u201B", "'",
	).Replace(s)
	// Smart double quotes → "
	s = strings.NewReplacer(
		"\u201C", "\"", "\u201D", "\"", "\u201E", "\"", "\u201F", "\"",
	).Replace(s)
	// Dashes/hyphens/minus → -
	s = strings.NewReplacer(
		"\u2010", "-", "\u2011", "-", "\u2012", "-",
		"\u2013", "-", "\u2014", "-", "\u2015", "-",
		"\u2212", "-",
	).Replace(s)
	// Special spaces → regular space
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\u00A0':
			return ' '
		case r >= '\u2002' && r <= '\u200A':
			return ' '
		case r == '\u202F' || r == '\u205F' || r == '\u3000':
			return ' '
		default:
			return r
		}
	}, s)

	return s
}

// fuzzyFind returns the byte index of oldText in content, trying exact match
// first then fuzzy. content must already be LF-normalized.
func fuzzyFind(content, oldText string) (index int, usedFuzzy bool, fuzzyContent string) {
	// Exact match first.
	if idx := strings.Index(content, oldText); idx >= 0 {
		return idx, false, content
	}

	// Fuzzy fallback.
	fuzzy := normalizeForFuzzyMatch(content)
	fuzzyOld := normalizeForFuzzyMatch(oldText)
	if idx := strings.Index(fuzzy, fuzzyOld); idx >= 0 {
		return idx, true, fuzzy
	}
	return -1, false, content
}

// fuzzyCount returns how many times oldText appears in content, using fuzzy
// matching. content must already be LF-normalized.
func fuzzyCount(content, oldText string) int {
	fuzzy := normalizeForFuzzyMatch(content)
	fuzzyOld := normalizeForFuzzyMatch(oldText)
	if len(fuzzyOld) == 0 {
		return 0
	}
	n := 0
	off := 0
	for {
		idx := strings.Index(fuzzy[off:], fuzzyOld)
		if idx < 0 {
			break
		}
		n++
		off += idx + len(fuzzyOld)
	}
	return n
}

// editOp is one replacement operation.
type editOp struct {
	matchIndex, matchLength int
	newText                 string
}

// lfEdit holds an LF-normalized edit with a reference to its original index.
type lfEdit struct {
	oldText, newText string
	origIdx          int
}

// applyEdits matches every edit against the same LF-normalized base content.
// Edits are first tried exact; if any needs fuzzy matching, all matching moves
// to fuzzy-normalized space and unchanged lines are preserved from the original.
func applyEdits(lfContent string, edits []editPair) (baseContent, newContent string, _ error) {
	lfEdits := make([]lfEdit, len(edits))
	for i, e := range edits {
		lfEdits[i] = lfEdit{
			oldText: normalizeToLF(e.OldString),
			newText: normalizeToLF(e.NewString),
			origIdx: i,
		}
	}

	// Validate non-empty oldText.
	for i, e := range lfEdits {
		if len(e.oldText) == 0 {
			return "", "", fmt.Errorf("old_string in edits[%d] must not be empty", i)
		}
	}

	// First pass: check if any edit needs fuzzy matching.
	var anyFuzzy bool
	matchResults := make([]fuzzyMatchResult, len(lfEdits))
	baseForMatch := lfContent
	for i, e := range lfEdits {
		idx, usedFuzzy, fuzzyBase := fuzzyFind(baseForMatch, e.oldText)
		if idx < 0 {
			// Retry against fully fuzzy content.
			fuzzyBase = normalizeForFuzzyMatch(baseForMatch)
			idx, _, _ = fuzzyFind(fuzzyBase, e.oldText)
			if idx < 0 {
				return "", "", fmt.Errorf("old_string in edits[%d] not found in file", i)
			}
			usedFuzzy = true
		}
		if usedFuzzy {
			anyFuzzy = true
			matchResults[i] = fuzzyMatchResult{
				idx:         idx,
				usedFuzzy:   true,
				fuzzyBase:   fuzzyBase,
				matchLength: len(normalizeForFuzzyMatch(e.oldText)),
			}
		} else {
			matchResults[i] = fuzzyMatchResult{
				idx:         idx,
				usedFuzzy:   false,
				fuzzyBase:   baseForMatch,
				matchLength: len(e.oldText),
			}
		}
	}

	// If any edit needs fuzzy, re-match everything in fuzzy space for consistency.
	if anyFuzzy {
		baseForMatch = normalizeForFuzzyMatch(lfContent)
		for i, e := range lfEdits {
			idx, _, _ := fuzzyFind(baseForMatch, e.oldText)
			if idx < 0 {
				return "", "", fmt.Errorf("old_string in edits[%d] not found in file", i)
			}
			matchResults[i].idx = idx
			matchResults[i].fuzzyBase = baseForMatch
			matchResults[i].matchLength = len(normalizeForFuzzyMatch(e.oldText))
			matchResults[i].usedFuzzy = true
		}
	}

	// Check uniqueness: each old_string must appear exactly once.
	for i, e := range lfEdits {
		count := fuzzyCount(baseForMatch, e.oldText)
		if count == 0 {
			return "", "", fmt.Errorf("old_string in edits[%d] not found in file", i)
		}
		if count > 1 {
			return "", "", fmt.Errorf("old_string in edits[%d] matches %d times; add more surrounding context to make it unique", i, count)
		}
	}

	// Build replacements and sort by matchIndex, checking overlaps.
	ops := make([]editOp, len(lfEdits))
	for i, mr := range matchResults {
		ops[i] = editOp{
			matchIndex:  mr.idx,
			matchLength: mr.matchLength,
			newText:     lfEdits[i].newText,
		}
	}

	for i := 1; i < len(ops); i++ {
		for j := i; j > 0 && ops[j].matchIndex < ops[j-1].matchIndex; j-- {
			ops[j], ops[j-1] = ops[j-1], ops[j]
			matchResults[j], matchResults[j-1] = matchResults[j-1], matchResults[j]
		}
	}
	for i := 1; i < len(ops); i++ {
		if ops[i-1].matchIndex+ops[i-1].matchLength > ops[i].matchIndex {
			return "", "", fmt.Errorf(
				"edits[%d] and edits[%d] overlap; merge them into one edit or target disjoint regions",
				findOrigIdx(matchResults[i-1], lfEdits),
				findOrigIdx(matchResults[i], lfEdits))
		}
	}

	baseContent = lfContent
	if anyFuzzy {
		newContent = applyReplacementsPreserve(lfContent, baseForMatch, ops)
	} else {
		newContent = replaceAll(baseForMatch, ops)
	}

	if baseContent == newContent {
		return "", "", fmt.Errorf("no changes made; replacement produced identical content")
	}

	return baseContent, newContent, nil
}

// fuzzyMatchResult records how one edit matched.
type fuzzyMatchResult struct {
	idx, matchLength int
	usedFuzzy        bool
	fuzzyBase        string
}

// findOrigIdx maps a matchResult back to the original lfEdit index.
func findOrigIdx(mr fuzzyMatchResult, lfEdits []lfEdit) int {
	for _, e := range lfEdits {
		// We match by looking for the lfEdit whose fuzzy-normalized oldText
		// length matches mr.matchLength and whose position matches. This is
		// best-effort; in the overlapping case the error message just needs
		// to point at roughly the right indices.
		if len(normalizeForFuzzyMatch(e.oldText)) == mr.matchLength {
			return e.origIdx
		}
	}
	return 0
}

// replaceAll applies replacements in reverse order of matchIndex for stability.
func replaceAll(content string, ops []editOp) string {
	for i := len(ops) - 1; i >= 0; i-- {
		r := ops[i]
		content = content[:r.matchIndex] + r.newText + content[r.matchIndex+r.matchLength:]
	}
	return content
}

// applyReplacementsPreserve applies replacements matched against baseContent to
// originalContent, preserving unchanged line blocks byte-for-byte from the
// original. When baseContent is fuzzy-normalized, only lines touched by
// replacements are rewritten; all others come from the original.
func applyReplacementsPreserve(original, base string, ops []editOp) string {
	origLines := splitLinesKeepEndings(original)
	baseSpans := lineByteSpans(base)

	if len(origLines) != len(baseSpans) {
		// Fuzzy normalization changed the line structure — fall back to direct
		// replacement on the original content.
		return replaceAll(original, ops)
	}

	// Group overlapping replacement ranges.
	type lineGroup struct {
		start, end int
		ops        []editOp
	}
	var groups []lineGroup
	for _, op := range ops {
		startLine, endLine := replacementLines(baseSpans, op)
		if len(groups) > 0 {
			last := &groups[len(groups)-1]
			if startLine < last.end {
				last.end = max(last.end, endLine)
				last.ops = append(last.ops, op)
				continue
			}
		}
		groups = append(groups, lineGroup{startLine, endLine, []editOp{op}})
	}

	var out strings.Builder
	origLineIdx := 0
	for _, g := range groups {
		for ; origLineIdx < g.start; origLineIdx++ {
			out.WriteString(origLines[origLineIdx])
		}
		groupStart := baseSpans[g.start].start
		groupEnd := baseSpans[g.end-1].end
		// Adjust match indices to be relative to groupStart.
		adjusted := make([]editOp, len(g.ops))
		for i, op := range g.ops {
			adjusted[i] = editOp{
				matchIndex:  op.matchIndex - groupStart,
				matchLength: op.matchLength,
				newText:     op.newText,
			}
		}
		replaced := replaceAll(base[groupStart:groupEnd], adjusted)
		out.WriteString(replaced)
		origLineIdx = g.end
	}
	for ; origLineIdx < len(origLines); origLineIdx++ {
		out.WriteString(origLines[origLineIdx])
	}
	return out.String()
}

type byteSpan struct{ start, end int }

// lineByteSpans returns the [start, end) byte offsets of each line in content.
func lineByteSpans(content string) []byteSpan {
	lines := splitLinesKeepEndings(content)
	spans := make([]byteSpan, len(lines))
	off := 0
	for i, l := range lines {
		spans[i] = byteSpan{off, off + len(l)}
		off += len(l)
	}
	return spans
}

// splitLinesKeepEndings splits on \n, keeping the newline as part of the
// preceding line (except the last element when no trailing \n).
func splitLinesKeepEndings(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.SplitAfter(content, "\n")
	// SplitAfter("a\n") = ["a\n", ""], SplitAfter("a") = ["a"].
	// Drop the trailing empty string when content ends with \n.
	if len(parts) > 0 && strings.HasSuffix(content, "\n") {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// replacementLines returns the [startLine, endLine) half-open range for op.
func replacementLines(spans []byteSpan, op editOp) (int, int) {
	repStart := op.matchIndex
	repEnd := op.matchIndex + op.matchLength

	startLine := -1
	for i, s := range spans {
		if repStart >= s.start && repStart < s.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		return 0, len(spans)
	}

	endLine := startLine
	for endLine < len(spans) && spans[endLine].end <= repEnd {
		endLine++
	}
	return startLine, endLine + 1
}

// detectLineEnding returns the line-ending convention used: "\r\n" if CRLF
// appears before the first bare LF, otherwise "\n".
func detectLineEnding(content string) string {
	lfIdx := strings.IndexByte(content, '\n')
	if lfIdx == -1 {
		return "\n"
	}
	crlfIdx := strings.Index(content, "\r\n")
	if crlfIdx == -1 {
		return "\n"
	}
	if crlfIdx < lfIdx {
		return "\r\n"
	}
	return "\n"
}

// restoreLineEndings converts LF back to the original line ending.
func restoreLineEndings(content, le string) string {
	if le == "\r\n" {
		return strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content
}

// prepContent strips BOM, normalizes line endings, and returns the LF content
// plus the original line-ending convention.
func prepContent(raw string) (lfContent, lineEnding string) {
	lfContent = stripBomRunes(raw)
	le := detectLineEnding(lfContent)
	lfContent = normalizeToLF(lfContent)
	return lfContent, le
}
