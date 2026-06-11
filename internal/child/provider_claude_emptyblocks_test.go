package child

import (
	"encoding/json"
	"testing"
)

// TestClaudeBusFrames_SkipsEmptyTextAndThinking guards the crash where Fable 5
// (display:omitted) streams thinking blocks with EMPTY text: the daemon would
// emit {type:"thinking"} (the omitempty tag drops an empty `thinking`), and pi's
// TUI does c.thinking.trim() → "undefined is not an object". Empty text/thinking
// blocks must be skipped entirely, and no emitted block may lack its payload
// field.
func TestClaudeBusFrames_SkipsEmptyTextAndThinking(t *testing.T) {
	prov := (ClaudeProvider{}).Fresh()
	prov.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"s","model":"claude-fable-5"}`), 1)

	// An empty thinking block (Fable noise) + a real text block + an empty text block.
	line := []byte(`{"type":"assistant","session_id":"s","message":{"content":[` +
		`{"type":"thinking","thinking":""},` +
		`{"type":"text","text":"hi"},` +
		`{"type":"text","text":""}]}}`)
	frames := prov.BusFrames(line, 2)

	sawHi := false
	for _, raw := range frames {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("emitted frame is not valid JSON: %v (%s)", err, raw)
		}
		msg, ok := m["message"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, c := range content {
			blk, _ := c.(map[string]any)
			switch blk["type"] {
			case "thinking":
				v, present := blk["thinking"]
				if !present {
					t.Fatalf("thinking block missing 'thinking' field (would crash pi TUI): %v", blk)
				}
				if s, _ := v.(string); s == "" {
					t.Fatalf("emitted an empty thinking block; should be skipped: %v", blk)
				}
			case "text":
				v, present := blk["text"]
				if !present {
					t.Fatalf("text block missing 'text' field (would crash pi TUI): %v", blk)
				}
				s, _ := v.(string)
				if s == "" {
					t.Fatalf("emitted an empty text block; should be skipped: %v", blk)
				}
				if s == "hi" {
					sawHi = true
				}
			}
		}
	}
	if !sawHi {
		t.Fatal("expected the non-empty text block (\"hi\") to be emitted")
	}
}
