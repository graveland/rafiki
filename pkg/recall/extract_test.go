package recall

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multigres/testkit/assert"
)

func testBlocks(t *testing.T, bs ...map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(bs)
	assert.NewAborting(t).NoError(err)
	return b
}

func TestExtractDropsToolResultsKeepsArgs(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.StrContains(out, text, "text missing from")
	c.StrContains(out, "[tool bash]", "tool args missing from")
	c.NotStrContains(out, strings.Repeat("y", 100), "tool_result content leaked")
	bound := len(text) + ToolArgMaxChars + 50
	c.LessOrEqual(bound, len(out), "output")
}

func TestExtractSkipsCompactionAndThinking(t *testing.T) {
	c := assert.NewAborting(t)
	comp := Message{
		ConversationID: "c", Ordinal: 0, Role: "user", Kind: "compaction_summary",
		Content: testBlocks(t, map[string]any{"type": "text", "text": "earlier turns compacted away"}),
	}
	c.Eq("", Extract(comp), "compaction extract")
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
	c.StrContains(out, "visible answer", "text missing from")
	c.NotStrContains(out, "secret reasoning", "thinking leaked")

	toolsOnly := Message{
		ConversationID: "c", Ordinal: 2, Role: "user",
		Content: testBlocks(t, map[string]any{"type": "tool_result", "content": "big result body"}),
	}
	for _, em := range ExtractAll([]Message{toolsOnly}) {
		c.True(em.Skip, "tool-result-only message not skipped: %+v", em)
	}
}

// A kind='compaction_tail' row is a stored duplicate of a message that already
// sits before the horizon; like a compaction summary it must extract to
// nothing and be marked Skip, so no window is ever built from it.
func TestCompactionRecallExtractSkipsTail(t *testing.T) {
	c := assert.NewAborting(t)
	tail := Message{
		ConversationID: "c", Ordinal: 3, Role: "user", Kind: "compaction_tail",
		Content: testBlocks(t, map[string]any{"type": "text", "text": "a copy of an earlier turn"}),
	}
	c.Eq("", Extract(tail), "compaction_tail extract")
	all := ExtractAll([]Message{tail})
	if !all[0].Skip || all[0].Text != "" || all[0].Ordinal != 3 {
		t.Fatalf("compaction_tail not skipped: %+v", all[0])
	}
}

func TestExtractStringContent(t *testing.T) {
	m := Message{
		ConversationID: "c", Ordinal: 3, Role: "user",
		Content: json.RawMessage(`"plain string content"`),
	}
	assert.NewAborting(t).Eq("user: plain string content", Extract(m), "got")
	all := ExtractAll([]Message{m})
	if all[0].Skip || all[0].Text != "user: plain string content" {
		t.Fatalf("ExtractAll = %+v", all[0])
	}
}

func TestRenderContextShowsResultSize(t *testing.T) {
	c := assert.NewAborting(t)
	m := Message{
		ConversationID: "c", Ordinal: 4, Role: "assistant",
		Content: testBlocks(t,
			map[string]any{"type": "text", "text": "looked it up"},
			map[string]any{"type": "tool_result", "content": strings.Repeat("y", 3277)},
		),
	}
	out := RenderContext([]Message{m})
	c.StrContains(out, "→ result (3.2KB)", "missing size marker in")
	c.StrContains(out, "#4 assistant: looked it up", "missing ordinal prefix in")
	c.NotStrContains(out, strings.Repeat("y", 100), "result content leaked")
	c.NotEq(0, utf8.RuneCountInString(out), "empty render")
}
