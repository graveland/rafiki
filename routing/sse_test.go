package routing

import (
	"encoding/json"
	"testing"
)

func TestParseCapturedResponseSSE(t *testing.T) {
	// Minimal Anthropic SSE: message_start carries input/cache usage; message_delta
	// carries stop_reason + cumulative output_tokens; message_stop ends it.
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, err := ParseCapturedResponse("text/event-stream; charset=utf-8", []byte(sse))
	if err != nil {
		t.Fatalf("unexpected scanner error: %v", err)
	}
	if stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", stop)
	}
	if u.InputTokens != 100 || u.OutputTokens != 25 || u.CacheReadTokens != 40 || u.CacheCreationTokens != 10 {
		t.Errorf("usage = %+v", u)
	}
	// The stream is reassembled into a canonical JSON Message (never raw SSE), so
	// it stores cleanly into the JSONB response column.
	if !json.Valid(canonical) {
		t.Errorf("canonical response is not valid JSON: %s", canonical)
	}
	var reassembled struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(canonical, &reassembled); err != nil {
		t.Fatalf("canonical unmarshal: %v", err)
	}
	if reassembled.StopReason != "end_turn" {
		t.Errorf("reassembled stop_reason = %q", reassembled.StopReason)
	}
	if len(reassembled.Content) == 0 || reassembled.Content[0].Text != "hi" {
		t.Errorf("reassembled content did not accumulate the text delta: %+v", reassembled.Content)
	}
}

func TestParseCapturedResponseSSEMissingContentBlockStart(t *testing.T) {
	// A text_delta with no preceding content_block_start: the SDK can't attach it,
	// so reassembly yields no content. Capture must fall back to the raw body
	// rather than persist a valid-but-empty {"content":null} Message that would
	// masquerade as a real empty completion.
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"dropped"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	if err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	// stop + usage are parsed independently of reassembly and stay correct.
	if stop != "end_turn" || u.OutputTokens != 25 {
		t.Errorf("stop=%q out=%d, want end_turn/25", stop, u.OutputTokens)
	}
	if string(canonical) != sse {
		t.Errorf("canonical must fall back to the raw body on a reassembly gap; got %q", canonical)
	}
	if json.Valid(canonical) {
		t.Error("expected the raw (non-JSON) body, not a reassembled empty Message")
	}
}

func TestParseCapturedResponseJSON(t *testing.T) {
	body := `{"type":"message","stop_reason":"max_tokens","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	stop, u, canonical, err := ParseCapturedResponse("application/json", []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stop != "max_tokens" || u.InputTokens != 7 || u.OutputTokens != 3 {
		t.Errorf("stop=%q usage=%+v", stop, u)
	}
	// A non-SSE body is already a JSON Message; returned unchanged.
	if string(canonical) != body {
		t.Errorf("JSON body should pass through unchanged: %s", canonical)
	}
}

func TestParseCapturedResponseGarbageIsSafe(t *testing.T) {
	stop, u, _, _ := ParseCapturedResponse("text/event-stream", []byte("event: junk\ndata: not json\n\n"))
	if stop != "" || u.InputTokens != 0 {
		t.Errorf("garbage should yield zero values, got stop=%q usage=%+v", stop, u)
	}
}

func TestParseCapturedResponseTruncatedStream(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}` + "\n\n"
	stop, u, _, _ := ParseCapturedResponse("text/event-stream", []byte(sse))
	if stop != "" {
		t.Errorf("stop_reason = %q, want empty for truncated stream", stop)
	}
	if u.InputTokens != 100 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheCreationTokens != 0 {
		t.Errorf("truncated stream usage = %+v, want InputTokens=100 OutputTokens=0", u)
	}
}
