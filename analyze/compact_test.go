// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/timescale/rafiki/insights"
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
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return json.RawMessage(b)
}

func unmarshalBlocks(t *testing.T, content json.RawMessage) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	return blocks
}

func TestCompact_HugeToolResultElided(t *testing.T) {
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

	if !reflect.DeepEqual(transcript, before) {
		t.Fatalf("Compact mutated input transcript")
	}
	if len(out.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(out.Turns))
	}
	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tr := block(blocks[0])
	elided, ok := tr["content"].(string)
	if !ok {
		t.Fatalf("expected elided content to be a string, got %T", tr["content"])
	}
	if len(elided) >= len(huge) {
		t.Fatalf("expected elided content to be shorter than original: %d vs %d", len(elided), len(huge))
	}
	if !strings.Contains(elided, "elided") {
		t.Fatalf("expected elision marker in content: %q", elided[:min(200, len(elided))])
	}
	// head 2/3 + tail 1/3 of the budget, split around a marker.
	if !strings.HasPrefix(elided, "XXX") {
		t.Fatalf("expected elided content to start with head bytes, got %q", elided[:min(50, len(elided))])
	}
	if !strings.HasSuffix(elided, "XXX") {
		t.Fatalf("expected elided content to end with tail bytes")
	}
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

	if !reflect.DeepEqual(transcript, before) {
		t.Fatalf("Compact mutated input transcript")
	}
	blocks := unmarshalBlocks(t, out.Turns[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	img := block(blocks[1])
	if img["type"] != "image" {
		t.Fatalf("expected image block preserved as type image, got %v", img)
	}
	src, ok := img["source"]
	if ok {
		t.Fatalf("expected image source removed/elided, got %v", src)
	}
	if img["elided"] != "[image elided]" {
		t.Fatalf("expected elided marker on image block, got %v", img)
	}
}

func TestCompact_MiddleTurnCompaction(t *testing.T) {
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
	if originalBytes <= policy.MaxTranscriptBytes {
		t.Fatalf("test setup bug: original %d bytes does not exceed ceiling %d", originalBytes, policy.MaxTranscriptBytes)
	}

	out := Compact(transcript, policy)

	if !reflect.DeepEqual(transcript, before) {
		t.Fatalf("Compact mutated input transcript")
	}

	var totalContentBytes int
	ordinalsPresent := map[int]bool{}
	for _, turn := range out.Turns {
		totalContentBytes += len(turn.Content)
		ordinalsPresent[turn.Ordinal] = true
	}
	if totalContentBytes > policy.MaxTranscriptBytes {
		t.Fatalf("expected total content bytes <= %d, got %d", policy.MaxTranscriptBytes, totalContentBytes)
	}

	for i := 1; i <= policy.KeepFirstTurns; i++ {
		if !ordinalsPresent[i] {
			t.Fatalf("expected first turn %d to be kept", i)
		}
	}
	for i := nTurns - policy.KeepLastTurns + 1; i <= nTurns; i++ {
		if !ordinalsPresent[i] {
			t.Fatalf("expected last turn %d to be kept", i)
		}
	}
	if !ordinalsPresent[50] {
		t.Fatalf("expected error turn 50 to be kept")
	}
	if !ordinalsPresent[60] {
		t.Fatalf("expected skill turn 60 to be kept")
	}

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
	if !foundMarker {
		t.Fatalf("expected at least one elision-marker turn shaped as a content-block array")
	}
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
	if !reflect.DeepEqual(transcript, before) {
		t.Fatalf("Compact mutated input transcript")
	}
}

func TestCompact_RuneBoundarySafe(t *testing.T) {
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
	if !ok {
		t.Fatalf("expected string content")
	}
	if !utf8.ValidString(elided) {
		t.Fatalf("elided content is not valid utf-8: %q", elided)
	}
}

func deepCopyTranscript(t *testing.T, tr *insights.Transcript) *insights.Transcript {
	t.Helper()
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal for deep copy: %v", err)
	}
	var out insights.Transcript
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal for deep copy: %v", err)
	}
	return &out
}
