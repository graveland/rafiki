package recall

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func testBlocks(t *testing.T, bs ...map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExtractDropsToolResultsKeepsArgs(t *testing.T) {
	text := "running the build now"
	m := Message{
		ConversationID: "c1",
		Ordinal:        0,
		Role:           "assistant",
		Content: testBlocks(t,
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "tool_use", "name": "bash", "input": map[string]any{"command": strings.Repeat("x", 5000)}},
			map[string]any{"type": "tool_result", "content": strings.Repeat("y", 9000)},
		),
	}
	out := Extract(m)
	if !strings.Contains(out, text) {
		t.Fatalf("text missing from %q", out)
	}
	if !strings.Contains(out, "[tool bash]") {
		t.Fatalf("tool args missing from %q", out)
	}
	if strings.Contains(out, strings.Repeat("y", 100)) {
		t.Fatalf("tool_result content leaked: %q", out)
	}
	bound := len(text) + ToolArgMaxChars + 50
	if len(out) > bound {
		t.Fatalf("output %d bytes exceeds bound %d", len(out), bound)
	}
}

func TestExtractSkipsCompactionAndThinking(t *testing.T) {
	comp := Message{
		ConversationID: "c", Ordinal: 0, Role: "user", Kind: "compaction_summary",
		Content: testBlocks(t, map[string]any{"type": "text", "text": "earlier turns compacted away"}),
	}
	if got := Extract(comp); got != "" {
		t.Fatalf("compaction extract = %q, want empty", got)
	}
	all := ExtractAll([]Message{comp})
	if !all[0].Skip || all[0].Text != "" || all[0].Ordinal != 0 {
		t.Fatalf("compaction not skipped: %+v", all[0])
	}

	m := Message{
		ConversationID: "c", Ordinal: 1, Role: "assistant",
		Content: testBlocks(t,
			map[string]any{"type": "thinking", "thinking": "secret reasoning about the plan"},
			map[string]any{"type": "redacted_thinking", "data": "zz"},
			map[string]any{"type": "text", "text": "visible answer"},
		),
	}
	out := Extract(m)
	if !strings.Contains(out, "visible answer") {
		t.Fatalf("text missing from %q", out)
	}
	if strings.Contains(out, "secret reasoning") {
		t.Fatalf("thinking leaked: %q", out)
	}

	toolsOnly := Message{
		ConversationID: "c", Ordinal: 2, Role: "user",
		Content: testBlocks(t, map[string]any{"type": "tool_result", "content": "big result body"}),
	}
	for _, em := range ExtractAll([]Message{toolsOnly}) {
		if !em.Skip {
			t.Fatalf("tool-result-only message not skipped: %+v", em)
		}
	}
}

func TestExtractStringContent(t *testing.T) {
	m := Message{
		ConversationID: "c", Ordinal: 3, Role: "user",
		Content: json.RawMessage(`"plain string content"`),
	}
	if got := Extract(m); got != "user: plain string content" {
		t.Fatalf("got %q", got)
	}
	all := ExtractAll([]Message{m})
	if all[0].Skip || all[0].Text != "user: plain string content" {
		t.Fatalf("ExtractAll = %+v", all[0])
	}
}

func TestRenderContextShowsResultSize(t *testing.T) {
	m := Message{
		ConversationID: "c", Ordinal: 4, Role: "assistant",
		Content: testBlocks(t,
			map[string]any{"type": "text", "text": "looked it up"},
			map[string]any{"type": "tool_result", "content": strings.Repeat("y", 3277)},
		),
	}
	out := RenderContext([]Message{m})
	if !strings.Contains(out, "→ result (3.2KB)") {
		t.Fatalf("missing size marker in %q", out)
	}
	if !strings.Contains(out, "#4 assistant: looked it up") {
		t.Fatalf("missing ordinal prefix in %q", out)
	}
	if strings.Contains(out, strings.Repeat("y", 100)) {
		t.Fatalf("result content leaked: %q", out)
	}
	if utf8.RuneCountInString(out) == 0 {
		t.Fatal("empty render")
	}
}
