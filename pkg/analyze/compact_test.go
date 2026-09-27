// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/insights"

	"github.com/multigres/testkit/assert"
)

func block(v any) map[string]any {
	b, ok := v.(map[string]any)
	if !ok {
		panic("block: not a map")
	}
	return b
}

func marshalBlocks(t *testing.T, blocks []map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(blocks)
	assert.NewAborting(t).NoError(err, "marshal blocks")
	return json.RawMessage(b)
}

func unmarshalBlocks(t *testing.T, content json.RawMessage) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	assert.NewAborting(t).NoError(json.Unmarshal(content, &blocks), "unmarshal blocks")
	return blocks
}

func TestCompact_HugeToolResultElided(t *testing.T) {
	c := assert.NewAborting(t)
	huge := strings.Repeat("X", 10_000)
	content := marshalBlocks(t, []map[string]any{
		{"type": "tool_result", "content": huge},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	before := deepCopyTranscript(t, transcript)

	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	out := Compact(transcript, policy)

	c.EqDiff(before, transcript, "Compact mutated input transcript")
	c.Len(out.Turns, 1, "expected 1 turn, got %d", len(out.Turns))
	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	c.Len(blocks, 1, "expected 1 block, got %d", len(blocks))
	tr := block(blocks[0])
	elided, ok := tr["content"].(string)
	if !ok {
		t.Fatalf("expected elided content to be a string, got %T", tr["content"])
	}
	c.Less(len(huge), len(elided), "expected elided content to be shorter than original")
	if !strings.Contains(elided, "elided") {
		t.Fatalf("expected elision marker in content: %q", elided[:min(200, len(elided))])
	}
	// head 2/3 + tail 1/3 of the budget, split around a marker.
	if !strings.HasPrefix(elided, "XXX") {
		t.Fatalf("expected elided content to start with head bytes, got %q", elided[:min(50, len(elided))])
	}
	c.True(strings.HasSuffix(elided, "XXX"), "expected elided content to end with tail bytes")
}

func TestCompact_ToolResultUnderBudgetUntouched(t *testing.T) {
	small := strings.Repeat("y", 100)
	content := marshalBlocks(t, []map[string]any{
		{"type": "tool_result", "content": small},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	out := Compact(transcript, policy)

	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	tr := block(blocks[0])
	if tr["content"] != small {
		t.Fatalf("expected untouched content, got %v", tr["content"])
	}
}

func TestCompact_ToolResultArrayContentElided(t *testing.T) {
	huge := strings.Repeat("Z", 10_000)
	content := marshalBlocks(t, []map[string]any{
		{
			"type": "tool_result",
			"content": []map[string]any{
				{"type": "text", "text": huge},
			},
		},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	out := Compact(transcript, policy)

	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	tr := block(blocks[0])
	inner, ok := tr["content"].([]any)
	if !ok {
		t.Fatalf("expected array content preserved, got %T", tr["content"])
	}
	textBlock := block(inner[0])
	txt, ok := textBlock["text"].(string)
	if !ok || len(txt) >= len(huge) || !strings.Contains(txt, "elided") {
		t.Fatalf("expected inner text block elided, got %v", textBlock["text"])
	}
}

func TestCompact_ImageElided(t *testing.T) {
	c := assert.NewAborting(t)
	content := marshalBlocks(t, []map[string]any{
		{"type": "text", "text": "look at this"},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": strings.Repeat("A", 5000)}},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	before := deepCopyTranscript(t, transcript)

	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	out := Compact(transcript, policy)

	c.EqDiff(before, transcript, "Compact mutated input transcript")
	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	c.Len(blocks, 2, "expected 2 blocks, got %d", len(blocks))
	img := block(blocks[1])
	c.False(img["type"] != "image", "expected image block preserved as type image, got %v", img)
	src, ok := img["source"]
	c.False(ok, "expected image source removed/elided, got %v", src)
	c.False(img["elided"] != "[image elided]", "expected elided marker on image block, got %v", img)
}

func TestCompact_MiddleTurnCompaction(t *testing.T) {
	c := assert.NewAborting(t)
	const nTurns = 100
	transcript := &insights.Transcript{Turns: make([]insights.TranscriptTurn, 0, nTurns)}
	for i := 1; i <= nTurns; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		content := marshalBlocks(t, []map[string]any{
			{"type": "text", "text": fmt.Sprintf("turn %d body padding %s", i, strings.Repeat("p", 2000)), "is_error": i == 50},
		})
		turn := insights.TranscriptTurn{Ordinal: i, Role: role, Content: content}
		if i == 60 {
			turn.Skills = []string{"some-skill"}
		}
		transcript.Turns = append(transcript.Turns, turn)
	}
	before := deepCopyTranscript(t, transcript)

	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 100_000, KeepFirstTurns: 4, KeepLastTurns: 20}

	var originalBytes int
	for _, turn := range transcript.Turns {
		originalBytes += len(turn.Content)
	}
	c.Greater(policy.MaxTranscriptBytes, originalBytes, "test setup bug: original")

	out := Compact(transcript, policy)

	c.EqDiff(before, transcript, "Compact mutated input transcript")

	var totalContentBytes int
	ordinalsPresent := map[int]bool{}
	for _, turn := range out.Turns {
		totalContentBytes += len(turn.Content)
		ordinalsPresent[turn.Ordinal] = true
	}
	c.LessOrEqual(policy.MaxTranscriptBytes, totalContentBytes, "expected total content bytes <")

	for i := 1; i <= policy.KeepFirstTurns; i++ {
		c.False(!ordinalsPresent[i], "expected first turn %d to be kept", i)
	}
	for i := nTurns - policy.KeepLastTurns + 1; i <= nTurns; i++ {
		c.False(!ordinalsPresent[i], "expected last turn %d to be kept", i)
	}
	c.False(!ordinalsPresent[50], "expected error turn 50 to be kept")
	c.False(!ordinalsPresent[60], "expected skill turn 60 to be kept")

	// There should be at least one synthetic elision marker turn.
	foundMarker := false
	for _, turn := range out.Turns {
		if turn.Role != "user" {
			continue
		}
		var blocks []map[string]any
		if err := json.Unmarshal(turn.Content, &blocks); err != nil || len(blocks) != 1 {
			continue
		}
		b := blocks[0]
		text, _ := b["text"].(string)
		if b["type"] == "text" && strings.Contains(text, "elided by compaction") {
			foundMarker = true
		}
	}
	c.True(foundMarker, "expected at least one elision-marker turn shaped as a content-block array")
}

func TestCompact_InputNeverMutated(t *testing.T) {
	content := marshalBlocks(t, []map[string]any{
		{"type": "tool_result", "content": strings.Repeat("Q", 10_000)},
		{"type": "image", "source": map[string]any{"type": "base64", "data": strings.Repeat("B", 1000)}},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	before := deepCopyTranscript(t, transcript)
	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	_ = Compact(transcript, policy)
	assert.NewAborting(t).EqDiff(before, transcript, "Compact mutated input transcript")
}

func TestCompact_RuneBoundarySafe(t *testing.T) {
	c := assert.NewAborting(t)
	// Multi-byte rune positioned right at the head/tail split boundary.
	huge := strings.Repeat("a", 1365) + "€" + strings.Repeat("b", 10_000)
	content := marshalBlocks(t, []map[string]any{
		{"type": "tool_result", "content": huge},
	})
	transcript := &insights.Transcript{
		Turns: []insights.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: content},
		},
	}
	policy := CompactPolicy{MaxToolResultBytes: 2048, MaxTranscriptBytes: 300 << 10, KeepFirstTurns: 4, KeepLastTurns: 20}
	out := Compact(transcript, policy)

	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	tr := block(blocks[0])
	elided, ok := tr["content"].(string)
	c.True(ok, "expected string content")
	c.True(utf8.ValidString(elided), "elided content is not valid utf-8: %q", elided)
}

func deepCopyTranscript(t *testing.T, tr *insights.Transcript) *insights.Transcript {
	t.Helper()
	c := assert.NewAborting(t)
	b, err := json.Marshal(tr)
	c.NoError(err, "marshal for deep copy")
	var out insights.Transcript
	c.NoError(json.Unmarshal(b, &out), "unmarshal for deep copy")
	return &out
}
