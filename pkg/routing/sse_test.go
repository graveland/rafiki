// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestParseCapturedResponseSSE(t *testing.T) {
	c := assert.NewCollecting(t)
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
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream; charset=utf-8", []byte(sse))
	c.Require().NoError(err, "unexpected scanner error")
	c.Eq("end_turn", stop, "stop_reason")
	c.False(u.InputTokens != 100 || u.OutputTokens != 25 || u.CacheReadTokens != 40 || u.CacheCreationTokens != 10, "usage = %+v", u)
	c.Eq("claude-sonnet-5", u.Model, "served model")
	// The stream is reassembled into a canonical JSON Message (never raw SSE), so
	// it stores cleanly into the JSONB response column.
	c.True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
	var reassembled struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &reassembled), "canonical unmarshal")
	c.Eq("end_turn", reassembled.StopReason, "reassembled stop_reason =")
	c.False(len(reassembled.Content) == 0 || reassembled.Content[0].Text != "hi", "reassembled content did not accumulate the text delta: %+v", reassembled.Content)
}

func TestParseCapturedResponseSSEMissingContentBlockStart(t *testing.T) {
	c := assert.NewCollecting(t)
	// A text_delta with no preceding content_block_start: the SDK can't attach
	// it, so that text is dropped from the canonical message. The stream IS
	// Anthropic wire format though (message_start accumulated), so the turn is
	// a real completion: persist the accumulated Message — a content-less
	// skeleton faithfully reflects a stream that produced no attachable
	// content (e.g. max_tokens before the first block).
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"dropped"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "well-formed stream must parse")
	c.False(stop != "end_turn" || u.OutputTokens != 25, "stop=%q out=%d, want end_turn/25", stop, u.OutputTokens)
	c.True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
}

func TestParseCapturedResponseJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	body := `{"type":"message","model":"moonshotai/kimi-k3","stop_reason":"max_tokens","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	stop, u, canonical, _, err := ParseCapturedResponse("application/json", []byte(body))
	c.Require().NoError(err, "unexpected error")
	c.False(stop != "max_tokens" || u.InputTokens != 7 || u.OutputTokens != 3, "stop=%q usage=%+v", stop, u)
	c.Eq("moonshotai/kimi-k3", u.Model, "served model")
	// A non-SSE body is already a JSON Message; returned unchanged.
	c.Eq(body, string(canonical), "JSON body should pass through unchanged: %s", canonical)
}

func TestParseCapturedResponseGarbageIsSafe(t *testing.T) {
	c := assert.NewCollecting(t)
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte("event: junk\ndata: not json\n\n"))
	c.Error(err, "garbage stream must be a parse error, not a zero-usage completion")
	c.Nil(canonical, "canonical must be nil on garbage; got")
	c.False(stop != "" || u.InputTokens != 0, "garbage should yield zero values, got stop=%q usage=%+v", stop, u)
}

func TestParseCapturedResponseTruncatedStream(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}` + "\n\n"
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	// message_start accumulated, so the (content-less) Message persists; the
	// usage extracted so far stays available to the caller.
	c.Require().NoError(err, "message_start-only stream must still persist")
	c.True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
	c.Eq("", stop, "stop_reason")
	c.False(u.InputTokens != 100 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheCreationTokens != 0, "truncated stream usage = %+v, want InputTokens=100 OutputTokens=0", u)
}

func TestParseCapturedResponseNonJSONBodyIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	// A gateway error page delivered with a success status and a non-SSE
	// content type: not a Message, and must not be stored as a completion.
	_, _, canonical, _, err := ParseCapturedResponse("text/plain", []byte("error code: 521"))
	c.Error(err, "non-JSON body must be a parse error")
	c.Nil(canonical, "canonical must be nil on a non-JSON body; got")
}

func TestParseCapturedResponsePingsDoNotBreakReassembly(t *testing.T) {
	c := assert.NewCollecting(t)
	// Anthropic interleaves keep-alive pings into long turns; they must pass
	// through the tee untouched (covered by the proxy fidelity tests) and be
	// ignored by reassembly.
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
		"event: ping\n" +
		`data: {"type":"ping"}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: ping\n" +
		`data: {"type":"ping"}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "ping-laden stream must parse")
	c.False(stop != "end_turn" || u.OutputTokens != 2, "stop=%q usage=%+v", stop, u)
	c.True(json.Valid(canonical), "canonical not valid JSON: %s", canonical)
}

// TestParseCapturedResponseProviderJSON proves the non-standard OpenRouter
// "provider" field survives the non-streaming parse path.
func TestParseCapturedResponseProviderJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"type":"message","model":"deepseek/deepseek-v4-pro","stop_reason":"end_turn",` +
		`"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":0},"provider":"CoreWeave"}`)
	_, u, _, _, err := ParseCapturedResponse("application/json", body)
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("CoreWeave", u.Provider, "Provider")
}

// TestParseCapturedResponseProviderSSE proves the same for the streaming path,
// where "provider" rides on message_start only — the final message_delta
// carries the usage but never the provider.
func TestParseCapturedResponseProviderSSE(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"deepseek/deepseek-v4-pro","usage":{"input_tokens":0,"output_tokens":0},"provider":"Novita"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":4}}` + "\n\n")
	_, u, _, _, err := ParseCapturedResponse("text/event-stream", body)
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("Novita", u.Provider, "Provider")
	c.Eq(4, u.CacheReadTokens, "CacheReadTokens")
}

// TestParseCapturedResponseSSERepairsMalformedToolInput reproduces a real
// Anthropic stream (rafikid log, 2026-09-06): a spurious duplicate
// input_json_delta chunk (an extra ", " immediately before the next chunk,
// which itself starts with ", ") produces a double comma, which is not valid
// JSON. The SDK's accumulator concatenates partial_json chunks with no
// validation, so the tool_use block's Input ends up unparseable even though
// the turn completed normally and the client already received a correct
// response. Losing the whole turn's usage over one corrupted tool call would
// silently drop billing data — the block must be repaired, not the turn
// discarded.
func TestParseCapturedResponseSSERepairsMalformedToolInput(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":2,"cache_read_input_tokens":24607,"cache_creation_input_tokens":364,"output_tokens":20}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Read","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"/a.go\", \"offset\": 140"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":", "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":", \"limit\": 100"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":118}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	stop, u, canonical, repaired, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "a malformed tool_use input must be repaired, not fail the whole turn")
	c.False(stop != "tool_use" || u.OutputTokens != 118 || u.InputTokens != 2, "usage lost despite independent extraction: stop=%q usage=%+v", stop, u)
	c.Require().True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
	c.Require().Len(repaired, 1, "expected exactly one repaired block, got")
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &msg), "canonical unmarshal")
	c.Require().False(len(msg.Content) != 1 || msg.Content[0].Name != "Read", "tool_use block missing from canonical: %+v", msg.Content)
	if !json.Valid(msg.Content[0].Input) {
		t.Errorf("sanitized input must still be valid JSON: %s", msg.Content[0].Input)
	}
}

// TestParseCapturedResponseNoProvider proves a native Anthropic response, which
// carries no such field, leaves Provider empty rather than inventing one.
func TestParseCapturedResponseNoProvider(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"type":"message","model":"claude-opus-4-8","stop_reason":"end_turn",` +
		`"usage":{"input_tokens":5,"output_tokens":1}}`)
	_, u, _, _, err := ParseCapturedResponse("application/json", body)
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("", u.Provider, "Provider")
}
