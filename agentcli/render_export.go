package agentcli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/timescale/rafiki/insights"
)

// RenderTranscriptMD renders an exported conversation as markdown: scalar
// header fields, then each turn with its role and content blocks.
// Shape-tolerant: unknown block types fall back to compact JSON, including
// Compact's synthetic elision-marker turns (a plain text block).
func RenderTranscriptMD(w io.Writer, t *insights.Transcript) error {
	if t == nil {
		_, err := fmt.Fprintln(w, "no transcript to render")
		return err
	}
	ew := &errWriter{w: w}
	ew.printf("# Conversation %s\n\n", t.ConversationID)
	if t.Owner != "" {
		ew.printf("- **owner:** %s\n", t.Owner)
	}
	if t.Persona != "" {
		ew.printf("- **persona:** %s\n", t.Persona)
	}
	if t.Source != "" {
		ew.printf("- **source:** %s\n", t.Source)
	}
	if t.DrivenBy != "" {
		ew.printf("- **driven_by:** %s\n", t.DrivenBy)
	}
	if len(t.AvailableSkills) > 0 {
		ew.printf("- **available_skills:** %s\n", strings.Join(t.AvailableSkills, ", "))
	}

	for i, turn := range t.Turns {
		ew.printf("\n## %d. %s\n\n", i+1, turn.Role)
		if err := renderTranscriptContent(w, turn.Content); err != nil {
			return err
		}
		if len(turn.Skills) > 0 {
			ew.printf("\n_skills: %s_\n", strings.Join(turn.Skills, ", "))
		}
	}
	return ew.err
}

// renderTranscriptContent renders a turn's raw content: a plain JSON string
// bare, a content-block array (text, tool_use as a one-liner, tool_result,
// thinking, and anything else as compact JSON), or — for any other shape —
// compact JSON.
func renderTranscriptContent(w io.Writer, content json.RawMessage) error {
	if len(content) == 0 {
		return nil
	}
	ew := &errWriter{w: w}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		ew.println(s)
		return ew.err
	}
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		ew.println(compactJSON(json.RawMessage(content)))
		return ew.err
	}
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			ew.printf("%v\n", b["text"])
		case "tool_use":
			ew.printf("**tool_use** `%v` %s\n", b["name"], compactJSON(b["input"]))
		case "tool_result":
			ew.printf("**tool_result** %s\n", compactJSON(b["content"]))
		case "thinking":
			ew.printf("_thinking_ %v\n", b["thinking"])
		default:
			ew.println(compactJSON(b))
		}
	}
	return ew.err
}

const cellMaxLen = 60

// truncateCell cuts s to cellMaxLen runes (not bytes — snippets are
// arbitrary user text and a byte slice can split a multi-byte rune).
func truncateCell(s string) string {
	if utf8.RuneCountInString(s) <= cellMaxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:cellMaxLen-1]) + "…"
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
