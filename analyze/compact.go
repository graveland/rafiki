package analyze

import (
	"encoding/json"
	"fmt"

	"github.com/timescale/rafiki/insights"
)

// Compact reduces a transcript to fit within p's byte budgets. It never
// mutates t: it returns a fresh Transcript built from a deep copy of t's
// turns.
//
// Two independent passes:
//  1. Per turn, per content block: oversized tool_result content is elided
//     head/tail, and any image (base64) source is replaced with a marker.
//     This runs on every turn regardless of the whole-transcript size.
//  2. If the transcript (measured as the sum of turn content bytes, after
//     pass 1) still exceeds MaxTranscriptBytes, middle turns are dropped,
//     keeping the first KeepFirstTurns, the last KeepLastTurns, and any
//     middle turn that contains an is_error block or invoked a skill. Each
//     contiguous dropped run collapses into one synthetic elision-marker
//     turn.
func Compact(t *insights.Transcript, p CompactPolicy) *insights.Transcript {
	out := &insights.Transcript{
		ConversationID:  t.ConversationID,
		Owner:           t.Owner,
		Persona:         t.Persona,
		Source:          t.Source,
		DrivenBy:        t.DrivenBy,
		AvailableSkills: append([]string(nil), t.AvailableSkills...),
		Turns:           make([]insights.TranscriptTurn, len(t.Turns)),
	}

	for i, turn := range t.Turns {
		out.Turns[i] = elideTurnBlocks(turn, p.MaxToolResultBytes)
	}

	out.Turns = compactMiddleTurns(out.Turns, p)
	return out
}

// elideTurnBlocks returns a copy of turn with oversized tool_result content
// and image sources elided. Non-block content (e.g. a plain JSON string) is
// copied through unchanged.
func elideTurnBlocks(turn insights.TranscriptTurn, maxToolResultBytes int) insights.TranscriptTurn {
	out := turn
	out.Skills = append([]string(nil), turn.Skills...)

	var blocks []map[string]any
	if err := json.Unmarshal(turn.Content, &blocks); err != nil {
		// Not a content-block array (e.g. a plain string); pass through.
		out.Content = append(json.RawMessage(nil), turn.Content...)
		return out
	}

	for i, b := range blocks {
		blocks[i] = elideBlock(b, maxToolResultBytes)
	}

	encoded, err := json.Marshal(blocks)
	if err != nil {
		out.Content = append(json.RawMessage(nil), turn.Content...)
		return out
	}
	out.Content = json.RawMessage(encoded)
	return out
}

// elideBlock elides a single content block in place (returning a new map):
// tool_result content over budget is head/tail elided; image sources are
// replaced with a marker.
func elideBlock(b map[string]any, maxToolResultBytes int) map[string]any {
	switch b["type"] {
	case "tool_result":
		b["content"] = elideToolResultContent(b["content"], maxToolResultBytes)
	case "image":
		if _, ok := b["source"]; ok {
			delete(b, "source")
			b["elided"] = "[image elided]"
		}
	}
	return b
}

// elideToolResultContent elides tool_result content, which is either a plain
// string or an array of content blocks (recursing into text/image blocks
// therein).
func elideToolResultContent(content any, maxToolResultBytes int) any {
	switch c := content.(type) {
	case string:
		return elideString(c, maxToolResultBytes)
	case []any:
		for i, e := range c {
			block, ok := e.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if s, ok := block["text"].(string); ok {
					block["text"] = elideString(s, maxToolResultBytes)
				}
			case "image":
				if _, ok := block["source"]; ok {
					delete(block, "source")
					block["elided"] = "[image elided]"
				}
			}
			c[i] = block
		}
		return c
	default:
		return content
	}
}

// elideString keeps the head 2/3 and tail 1/3 of maxBytes when s exceeds
// maxBytes, joined by a marker naming the elided size. Splits land on rune
// boundaries so the result is always valid UTF-8.
func elideString(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}

	headBudget := maxBytes * 2 / 3
	tailBudget := maxBytes - headBudget

	headEnd := runeFloor(s, headBudget)
	tailStart := runeCeil(s, len(s)-tailBudget)
	if tailStart < headEnd {
		tailStart = headEnd
	}

	elidedBytes := tailStart - headEnd
	elidedKB := elidedBytes / 1024
	if elidedBytes > 0 && elidedKB == 0 {
		elidedKB = 1
	}
	marker := fmt.Sprintf("[... %d KB elided ...]", elidedKB)
	return s[:headEnd] + marker + s[tailStart:]
}

// runeFloor returns the largest index <= n that does not split a UTF-8 rune.
func runeFloor(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && isUTF8Continuation(s[n]) {
		n--
	}
	return n
}

// runeCeil returns the smallest index >= n that does not split a UTF-8 rune.
func runeCeil(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for n < len(s) && isUTF8Continuation(s[n]) {
		n++
	}
	return n
}

// isUTF8Continuation reports whether b is a UTF-8 continuation byte (10xxxxxx),
// i.e. splitting immediately before it would cut a multi-byte rune in half.
func isUTF8Continuation(b byte) bool {
	return b&0xC0 == 0x80
}

// compactMiddleTurns drops middle turns once the transcript still exceeds
// MaxTranscriptBytes after block-level elision, keeping the first
// KeepFirstTurns, the last KeepLastTurns, and any middle turn that contains an
// is_error block or invoked a skill. Contiguous dropped runs collapse into one
// synthetic elision-marker turn.
func compactMiddleTurns(turns []insights.TranscriptTurn, p CompactPolicy) []insights.TranscriptTurn {
	if transcriptBytes(turns) <= p.MaxTranscriptBytes || len(turns) <= p.KeepFirstTurns+p.KeepLastTurns {
		return turns
	}

	n := len(turns)
	keep := make([]bool, n)
	for i := 0; i < n; i++ {
		switch {
		case i < p.KeepFirstTurns, i >= n-p.KeepLastTurns:
			keep[i] = true
		case len(turns[i].Skills) > 0, turnHasError(turns[i]):
			keep[i] = true
		}
	}

	out := make([]insights.TranscriptTurn, 0, n)
	i := 0
	for i < n {
		if keep[i] {
			out = append(out, turns[i])
			i++
			continue
		}
		start := i
		for i < n && !keep[i] {
			i++
		}
		out = append(out, elisionMarkerTurn(turns[start].Ordinal, turns[i-1].Ordinal))
	}
	return out
}

// transcriptBytes is the cheap whole-transcript size proxy: the sum of each
// turn's content bytes.
func transcriptBytes(turns []insights.TranscriptTurn) int {
	total := 0
	for _, t := range turns {
		total += len(t.Content)
	}
	return total
}

// turnHasError reports whether any content block in turn has "is_error":true.
func turnHasError(turn insights.TranscriptTurn) bool {
	var blocks []map[string]any
	if err := json.Unmarshal(turn.Content, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if isErr, ok := b["is_error"].(bool); ok && isErr {
			return true
		}
	}
	return false
}

// elisionMarkerTurn builds the synthetic turn that replaces a contiguous run
// of dropped turns with ordinals [first, last].
func elisionMarkerTurn(first, last int) insights.TranscriptTurn {
	text := fmt.Sprintf("[turns %d–%d elided by compaction]", first, last)
	encoded, _ := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	return insights.TranscriptTurn{Ordinal: first, Role: "user", Content: json.RawMessage(encoded)}
}
