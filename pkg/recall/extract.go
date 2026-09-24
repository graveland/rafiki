package recall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ExtractedMessage is one message's extraction result, ready for windowing.
type ExtractedMessage struct {
	Ordinal int
	Role    string
	Text    string
	Skip    bool
}

// Extract renders a captured message as searchable text: its content blocks
// joined with "\n", prefixed "role: ". Tool results, thinking blocks and
// unknown block types are dropped; tool_use arguments are compacted and
// truncated to ToolArgMaxChars; images and documents become markers. A
// compaction summary extracts to the empty string.
func Extract(m Message) string {
	return renderMessage(m, false)
}

// ExtractAll extracts every message in order. Compaction summaries and
// messages with no extractable text (e.g. tool results only) are marked Skip.
func ExtractAll(ms []Message) []ExtractedMessage {
	out := make([]ExtractedMessage, len(ms))
	for i, m := range ms {
		text := Extract(m)
		out[i] = ExtractedMessage{Ordinal: m.Ordinal, Role: m.Role, Text: text, Skip: text == ""}
	}
	return out
}

// RenderContext renders messages for context expansion: one line per message,
// prefixed "#<ordinal> ", with tool results shown as size markers only.
func RenderContext(ms []Message) string {
	lines := make([]string, 0, len(ms))
	for _, m := range ms {
		if text := renderMessage(m, true); text != "" {
			lines = append(lines, fmt.Sprintf("#%d %s", m.Ordinal, text))
		}
	}
	return strings.Join(lines, "\n")
}

// renderMessage extracts a message's text; with resultSizes, tool_result
// blocks render as "→ result (<size>)" markers instead of being dropped.
func renderMessage(m Message, resultSizes bool) string {
	if m.Kind == "compaction_summary" {
		return ""
	}
	parts := blockTexts(m.Content, resultSizes)
	if len(parts) == 0 {
		return ""
	}
	return m.Role + ": " + strings.Join(parts, "\n")
}

// blockTexts renders one content payload into text parts. A JSON string is
// one text block; anything unparseable is empty.
func blockTexts(content json.RawMessage, resultSizes bool) []string {
	if len(content) == 0 {
		return nil
	}
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		if str == "" {
			return nil
		}
		return []string{str}
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		var typ string
		if err := json.Unmarshal(b["type"], &typ); err != nil {
			continue
		}
		switch typ {
		case "text":
			var text string
			if err := json.Unmarshal(b["text"], &text); err == nil && text != "" {
				out = append(out, text)
			}
		case "tool_use":
			var name string
			_ = json.Unmarshal(b["name"], &name)
			out = append(out, toolUseText(name, b["input"]))
		case "tool_result":
			if resultSizes {
				out = append(out, "→ result ("+humanSize(contentLen(b["content"]))+")")
			}
		case "image":
			out = append(out, "[image]")
		case "document":
			out = append(out, "[document]")
		}
	}
	return out
}

// toolUseText renders a tool_use block: "[tool name] " plus the compacted
// input JSON, truncated to ToolArgMaxChars runes.
func toolUseText(name string, input json.RawMessage) string {
	args := "{}"
	if len(input) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, input); err == nil {
			args = buf.String()
		}
	}
	return "[tool " + name + "] " + truncateRunes(args, ToolArgMaxChars)
}

// contentLen measures a tool_result's content payload in bytes, compacted.
func contentLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return len(raw)
	}
	return buf.Len()
}

// truncateRunes cuts s to at most max runes, appending an ellipsis when cut.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := 0
	for i := range s {
		if cut == max {
			return s[:i] + "…"
		}
		cut++
	}
	return s
}

// humanSize renders a byte count as "812B", "3.2KB" or "1.4MB".
func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}
