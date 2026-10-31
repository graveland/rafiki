// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
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

// A content_block_delta for a block that never started is a reassembly gap, and
// on the proxy path capture is BEST-EFFORT: the client already has the bytes, so
// the delta is skipped and reported while the turn still persists with the usage
// the message_delta carried. Failing the capture over it would only lose the
// record and the billing row.
func TestParseCapturedResponseSSEDeltaWithoutBlockStartIsSkipped(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"dropped"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "a reassembly gap must not discard the accumulated turn")
	c.Eq("end_turn", stop, "stop_reason")
	c.False(u.InputTokens != 100 || u.OutputTokens != 25, "usage = %+v, want the counts the stream did report", u)
	c.Require().Len(diag.Anomalies, 1, "the skipped delta must be reported, got")
	c.StrContains(diag.Anomalies[0], "never started", "the report names the reason")
	c.Len(diag.Repaired, 0, "repaired blocks")
	c.True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
}

// TestParseCapturedResponseSSESkipsUnmodellableEvents pins the best-effort rule
// for the three shapes that used to fail the whole capture: an unsupported
// content delta, a delta for a block index that never started, and an in-band
// error event. Each one still stores the content and usage that accumulated, and
// reports the anomaly — a claude turn carrying a delta the codec does not model
// yet must not lose its conversation rows and its billing row.
func TestParseCapturedResponseSSESkipsUnmodellableEvents(t *testing.T) {
	const start = "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n"
	const block = "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	const stop0 = "event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	const delta = "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n"
	const end = "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	cases := []struct {
		name     string
		body     string
		wantStop string
		wantOut  int64
		wantIn   string // a substring of the reported anomaly
	}{
		{
			name: "unsupported content delta",
			body: start + block + "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"future_delta","text":"x"}}` + "\n\n" +
				stop0 + delta + end,
			wantStop: "end_turn", wantOut: 25, wantIn: "future_delta",
		},
		{
			name: "delta for a block index that never started",
			body: start + block + stop0 + "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"orphan"}}` + "\n\n" +
				delta + end,
			wantStop: "end_turn", wantOut: 25, wantIn: "never started",
		},
		{
			// An in-band error ends the stream where it arrives: the record is
			// what the provider had already sent.
			name: "in-band error event",
			body: start + block + "event: error\n" +
				`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
			wantStop: "", wantOut: 1, wantIn: "Overloaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			stop, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(tc.body))
			c.Require().NoError(err, "an event the codec cannot fold in must not discard the turn")
			c.Eq(tc.wantStop, stop, "stop_reason")
			c.Eq(int64(100), u.InputTokens, "input tokens from message_start")
			c.Eq(tc.wantOut, u.OutputTokens, "output tokens accumulated")
			c.Require().True(len(diag.Anomalies) > 0, "the skipped event must be reported")
			c.StrContains(diag.Anomalies[0], tc.wantIn, "the report names the reason")
			var stored struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
			c.Require().Len(stored.Content, 1, "the block that did accumulate is still stored")
			c.Eq("hi", stored.Content[0].Text, "the accumulated text survives the skipped event")
		})
	}
}

// A stream that legitimately produces no content block at all (max_tokens can
// stop a turn before the first one) is still a Message and persists as a
// content-less one — an empty content array, not a parse failure.
func TestParseCapturedResponseContentlessMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	stop, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "a content-less Message must parse")
	c.Eq("max_tokens", stop, "stop_reason")
	c.Eq(int64(25), u.OutputTokens, "output tokens")
	c.Len(diag.Repaired, 0, "repaired blocks")
	c.Len(diag.Anomalies, 0, "anomalies")
	var stored struct {
		Content []json.RawMessage `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Len(stored.Content, 0, "a content-less Message stores an empty content array")
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

// A body that ends after a complete event but before message_stop still
// persists: the proxy's copy loop is where a mid-stream cut is detected and the
// turn failed, so what reaches capture is whatever the upstream wrote, and the
// usage extracted so far is the caller's to keep.
func TestParseCapturedResponseTruncatedStream(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}` + "\n\n"
	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "message_start-only stream must still persist")
	c.True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
	c.Eq("", stop, "stop_reason")
	// No message_delta arrived, so output_tokens is the placeholder count
	// message_start itself carried — the accumulator records what the provider
	// reported rather than substituting a zero it never said.
	c.False(u.InputTokens != 100 || u.OutputTokens != 1 || u.CacheReadTokens != 0 || u.CacheCreationTokens != 0, "truncated stream usage = %+v, want InputTokens=100 OutputTokens=1", u)
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
// carries the usage but never the provider — and proves the delta's usage is
// backfilled into both CapturedUsage and the stored response.
func TestParseCapturedResponseProviderSSE(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"deepseek/deepseek-v4-pro","usage":{"input_tokens":0,"output_tokens":0},"provider":"Novita"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":4}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n")
	_, u, canonical, _, err := ParseCapturedResponse("text/event-stream", body)
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("Novita", u.Provider, "Provider")
	c.Eq(4, u.CacheReadTokens, "CacheReadTokens")
	var stored struct {
		Provider string           `json:"provider"`
		Usage    map[string]int64 `json:"usage"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Eq("Novita", stored.Provider, "stored provider")
	c.Eq(int64(5), stored.Usage["input_tokens"], "stored input tokens")
	c.Eq(int64(4), stored.Usage["cache_read_input_tokens"], "stored cache read tokens")
}

// TestParseCapturedResponseSSERepairsMalformedToolInput reproduces a real
// Anthropic stream (rafikid log, 2026-09-06): a spurious duplicate
// input_json_delta chunk (an extra ", " immediately before the next chunk,
// which itself starts with ", ") produces a double comma, which is not valid
// JSON. The accumulator concatenates partial_json chunks with no validation, so
// the tool_use block's Input ends up unparseable even though the turn completed
// normally and the client already received a correct response. Losing the whole
// turn's usage over one corrupted tool call would silently drop billing data —
// the block must be repaired, not the turn discarded.
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

	stop, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "a malformed tool_use input must be repaired, not fail the whole turn")
	c.False(stop != "tool_use" || u.OutputTokens != 118 || u.InputTokens != 2, "usage lost despite independent extraction: stop=%q usage=%+v", stop, u)
	c.Require().True(json.Valid(canonical), "canonical response is not valid JSON: %s", canonical)
	c.Len(diag.Anomalies, 0, "anomalies")
	c.Require().Len(diag.Repaired, 1, "expected exactly one repaired block, got")
	// The repair is reported AND the raw fragments are kept: the stored input is
	// a placeholder object carrying them, so a corrupted call stays forensically
	// readable after the turn.
	c.StrContains(diag.Repaired[0], `raw="{\"file_path\": \"/a.go\", \"offset\": 140, , \"limit\": 100}"`, "repair detail")
	var stored struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Require().Len(stored.Content, 1, "content blocks")
	c.Eq("Read", stored.Content[0].Name, "tool name")
	var input struct {
		Malformed string `json:"_rafiki_malformed_input"`
	}
	c.Require().NoError(json.Unmarshal(stored.Content[0].Input, &input), "sanitized input must still be valid JSON: %s", stored.Content[0].Input)
	c.Eq(`{"file_path": "/a.go", "offset": 140, , "limit": 100}`, input.Malformed, "raw fragments preserved")
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

// TestParseCapturedResponseSSERepairsMalformedServerToolInput pins the same
// repair for a server-tool block. Its input is part of the record: storing an
// empty object would claim the tool was called with no arguments, so the block
// keeps its own type and every other key while the input becomes the
// malformed-input placeholder carrying the fragments.
func TestParseCapturedResponseSSERepairsMalformedServerToolInput(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":4,"output_tokens":2}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{},"provider_metadata":{"source":"web"}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"rafiki\""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":", , "}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	_, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "a malformed server-tool input must be repaired, not fail the whole turn")
	c.Eq(int64(4), u.InputTokens, "usage is accumulated independently of the block")
	c.Require().Len(diag.Repaired, 1, "expected exactly one repaired block, got")
	c.StrContains(diag.Repaired[0], "server_tool_use", "the report names the block type")
	c.StrContains(diag.Repaired[0], `raw="{\"query\": \"rafiki\", ,"`, "the report carries the fragments")

	var stored struct {
		Content []struct {
			Type     string          `json:"type"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Metadata map[string]any  `json:"provider_metadata"`
		} `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Require().Len(stored.Content, 1, "content blocks")
	c.Eq("server_tool_use", stored.Content[0].Type, "the block keeps the server tool's own type")
	c.Eq("web_search", stored.Content[0].Name, "the tool name survives")
	c.Eq("web", stored.Content[0].Metadata["source"], "the block's other keys survive")
	var input struct {
		Malformed string `json:"_rafiki_malformed_input"`
	}
	c.Require().NoError(json.Unmarshal(stored.Content[0].Input, &input), "the placeholder must be valid JSON: %s", stored.Content[0].Input)
	c.Eq(`{"query": "rafiki", ,`, input.Malformed, "the raw fragments are preserved")
}

// TestParseCapturedResponseSSEKeepsUnmodelledStopReason pins that the proxy's
// captured record keeps a stop reason the codec does not model, in both the
// stop_reason column value and the stored response body. Erasing it to "" would
// lose the only signal of why the turn ended.
func TestParseCapturedResponseSSEKeepsUnmodelledStopReason(t *testing.T) {
	c := assert.NewCollecting(t)
	const wire = "model_context_window_exceeded"
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_ctx","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":2}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"` + wire + `"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	stop, _, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq(wire, stop, "the stop_reason column keeps the provider's own spelling")

	var stored struct {
		StopReason string `json:"stop_reason"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Eq(wire, stored.StopReason, "the stored response keeps the provider's own spelling")
}

// TestParseSSEUsageInMessageDelta pins OpenRouter's deepseek shape: the
// message_start reports zeros and the final message_delta carries the real
// counts. The accumulator's backfill must take every field the delta actually
// populates, in CapturedUsage and in the stored response alike.
func TestParseSSEUsageInMessageDelta(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_or","type":"message","role":"assistant","content":[],"model":"deepseek/deepseek-v4-pro","usage":{"input_tokens":0,"output_tokens":0},"provider":"DeepSeek"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	stop, u, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("end_turn", stop, "stop_reason")
	c.Eq(int64(11), u.InputTokens, "input tokens")
	c.Eq(int64(7), u.OutputTokens, "output tokens")
	c.Eq(int64(5), u.CacheReadTokens, "cache read tokens")
	c.Eq(int64(2), u.CacheCreationTokens, "cache creation tokens")
	c.Eq("DeepSeek", u.Provider, "provider")

	var stored struct {
		Usage map[string]int64 `json:"usage"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.EqDeep(map[string]int64{
		"input_tokens":                11,
		"output_tokens":               7,
		"cache_read_input_tokens":     5,
		"cache_creation_input_tokens": 2,
	}, stored.Usage, "stored usage")
}

// TestParseCapturedResponseCorpusPairedSSEAndJSON runs every Anthropic fixture of
// the golden corpus (task 3.1) through the proxy's two capture paths. The
// streaming and whole-body paths must produce the same stored content, and that
// content must be the codec's own decoded golden — which is what pins
// start-block residue (a tool's caller, a server tool's type and metadata),
// accumulated citations and the server-tool result blocks the client must never
// see routed to a client tool executor.
func TestParseCapturedResponseCorpusPairedSSEAndJSON(t *testing.T) {
	for _, name := range []string{"anthropic-caller", "anthropic-citations", "anthropic-servertool", "anthropic-tooluse"} {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			var golden struct {
				ID         string           `json:"id"`
				Model      string           `json:"model"`
				StopReason string           `json:"stop_reason"`
				Provider   string           `json:"provider"`
				Usage      map[string]int64 `json:"usage"`
				Content    json.RawMessage  `json:"content"`
			}
			c.Require().NoError(json.Unmarshal(msgtest.Golden(filepath.Join("golden", name+".sse.decoded.json")), &golden), "read the decoded golden")

			stop, usage, canonical, diag, err := ParseCapturedResponse("text/event-stream", msgtest.Golden(filepath.Join("responses", name+".sse")))
			c.Require().NoError(err, "streamed capture")
			c.Len(diag.Repaired, 0, "repaired blocks")
			c.Len(diag.Anomalies, 0, "a modelled stream skips nothing")

			var streamed struct {
				ID         string           `json:"id"`
				Type       string           `json:"type"`
				Role       string           `json:"role"`
				Model      string           `json:"model"`
				Provider   string           `json:"provider"`
				StopReason string           `json:"stop_reason"`
				Usage      map[string]int64 `json:"usage"`
				Content    json.RawMessage  `json:"content"`
			}
			c.Require().NoError(json.Unmarshal(canonical, &streamed), "stored response")
			// The stored body keeps the Anthropic message envelope these turns are
			// recorded under (protocol = 'anthropic').
			c.Eq("message", streamed.Type, "stored type")
			c.Eq("assistant", streamed.Role, "stored role")
			c.Eq(golden.ID, streamed.ID, "id")
			c.Eq(golden.Model, streamed.Model, "model")
			c.Eq(golden.Provider, streamed.Provider, "provider")
			c.Eq(golden.StopReason, streamed.StopReason, "stored stop_reason")
			c.Eq(golden.StopReason, stop, "returned stop_reason")
			c.Eq(golden.Usage["input"], streamed.Usage["input_tokens"], "stored input tokens")
			c.Eq(golden.Usage["output"], streamed.Usage["output_tokens"], "stored output tokens")
			c.Eq(golden.Usage["cache_read"], streamed.Usage["cache_read_input_tokens"], "stored cache read tokens")
			c.Eq(golden.Usage["cache_write"], streamed.Usage["cache_creation_input_tokens"], "stored cache write tokens")
			// The serving backend reaches CapturedUsage, which is what capture's
			// ServedProvider and ProviderGuard read.
			c.Eq(golden.Provider, usage.Provider, "CapturedUsage.Provider")
			c.Eq(golden.Model, usage.Model, "CapturedUsage.Model")
			c.Eq(golden.Usage["input"], usage.InputTokens, "CapturedUsage input tokens")
			c.Eq(golden.Usage["output"], usage.OutputTokens, "CapturedUsage output tokens")
			c.Eq(golden.Usage["cache_read"], usage.CacheReadTokens, "CapturedUsage cache read tokens")
			c.Eq(golden.Usage["cache_write"], usage.CacheCreationTokens, "CapturedUsage cache creation tokens")
			assertCanonicalJSON(t, golden.Content, streamed.Content, "streamed content vs the codec golden")

			// The whole-body path of the same fixture stores the same content, so a
			// non-streamed turn and its streamed twin are one recorded reply.
			_, wholeUsage, wholeCanonical, _, err := ParseCapturedResponse("application/json", msgtest.Golden(filepath.Join("responses", name+".json")))
			c.Require().NoError(err, "whole-body capture")
			var whole struct {
				Content json.RawMessage `json:"content"`
			}
			c.Require().NoError(json.Unmarshal(wholeCanonical, &whole), "whole-body response")
			assertCanonicalJSON(t, streamed.Content, whole.Content, "streamed content vs whole-body content")
			c.Eq(usage.Provider, wholeUsage.Provider, "provider on both paths")
			c.Eq(usage.InputTokens, wholeUsage.InputTokens, "input tokens on both paths")
		})
	}
}

// TestParseCapturedResponseSSEKeepsStartBlockResidue proves a block keeps what
// its content_block_start carried: a signature seeded before any
// signature_delta, and a tool call's caller residue.
func TestParseCapturedResponseSSEKeepsStartBlockResidue(t *testing.T) {
	c := assert.NewCollecting(t)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_residue","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","usage":{"input_tokens":7,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"sig-seeded"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_02","name":"mcp__rafiki__agent_spawn","input":{},"caller":{"type":"direct"}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"prompt\":\"run\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	_, _, canonical, _, err := ParseCapturedResponse("text/event-stream", []byte(sse))
	c.Require().NoError(err, "ParseCapturedResponse")
	var stored struct {
		Content []map[string]json.RawMessage `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Require().Len(stored.Content, 2, "content blocks")
	c.Eq(`"sig-seeded"`, string(stored.Content[0]["signature"]), "seeded signature")
	c.Eq(`"hmm"`, string(stored.Content[0]["thinking"]), "thinking text")
	c.Eq(`{"type":"direct"}`, string(stored.Content[1]["caller"]), "tool caller residue")
	c.Eq(`{"prompt":"run"}`, string(stored.Content[1]["input"]), "tool input")
}

// TestParseCapturedResponseSSEOverLongLineStopsTheCapture pins that a
// non-recoverable READ error ENDS the best-effort loop. bufio.Scanner hands back
// the same bufio.ErrTooLong on every Scan once a line exceeds the codec's 16 MiB
// maxStreamLine, so skipping and retrying would spin forever and the turn would
// never be persisted. The events accumulated before the bad line are still the
// record, and the read error is reported as the anomaly.
//
// The call runs in a goroutine behind a short deadline: a regression here is a
// hang, so the test must fail in seconds rather than time the suite out.
func TestParseCapturedResponseSSEOverLongLineStopsTheCapture(t *testing.T) {
	c := assert.NewCollecting(t)
	const prefix = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_toolong","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":11,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	// One data line past the decoder's maxStreamLine. Nothing after it is
	// reachable: the scanner is dead from this line on.
	overLong := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
		strings.Repeat("x", 20<<20) + `"}}` + "\n\n"
	body := []byte(prefix + overLong +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n")

	type captured struct {
		stop      string
		usage     CapturedUsage
		canonical []byte
		diag      CaptureDiagnostics
		err       error
	}
	done := make(chan captured, 1)
	go func() {
		stop, usage, canonical, diag, err := ParseCapturedResponse("text/event-stream", body)
		done <- captured{stop: stop, usage: usage, canonical: canonical, diag: diag, err: err}
	}()
	var got captured
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ParseCapturedResponse never returned: a sticky stream read error is being skipped and retried forever")
	}

	c.Require().NoError(got.err, "a read error must not fail the capture: the accumulated prefix is the record")
	c.Eq(int64(11), got.usage.InputTokens, "usage from the message_start before the bad line")
	c.Eq("claude-sonnet-5", got.usage.Model, "served model")
	c.Eq("", got.stop, "no message_delta was reached")
	c.Require().Len(got.diag.Anomalies, 1, "the read error is reported")
	c.StrContains(got.diag.Anomalies[0], "read stream", "the anomaly names the read failure")
	var stored struct {
		ID      string `json:"id"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(got.canonical, &stored), "canonical unmarshal")
	c.Eq("msg_toolong", stored.ID, "reassembled message id")
	c.Require().Len(stored.Content, 1, "the block that started before the bad line")
	c.Eq("hi", stored.Content[0].Text, "text accumulated before the bad line")
}

// TestParseCapturedResponseSSELongRunOfSkipsStillReachesTheTerminalEvents pins
// that the best-effort skip is NOT capped. A content-level skip consumes an
// event, so a finite body always terminates without a cap; a cap would instead
// cut the loop off mid-stream and discard the tail of a legitimate turn — the
// final message_delta and the usage it carries, which is the billing row. A long
// run of events the codec merely did not model must cost anomalies, never the
// stop reason or the final usage.
func TestParseCapturedResponseSSELongRunOfSkipsStillReachesTheTerminalEvents(t *testing.T) {
	c := assert.NewCollecting(t)
	const skips = 150
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_skips","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":11,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	// Each of these is a content-level refusal the codec reports per event, and
	// the decoder consumes every one of them: the loop advances.
	refusal := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"future_delta","text":"x"}}` + "\n\n"
	stream += strings.Repeat(refusal, skips)
	stream += "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":99}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	stop, u, canonical, diag, err := ParseCapturedResponse("text/event-stream", []byte(stream))
	c.Require().NoError(err, "ParseCapturedResponse")
	c.Eq("end_turn", stop, "the message_delta past the skips is still reached")
	c.Eq(int64(11), u.InputTokens, "usage from the message_start")
	c.Eq(int64(99), u.OutputTokens, "the final usage is not lost to the skips")
	c.Require().Len(diag.Anomalies, maxCaptureAnomalies+1, "the bounded reasons, then a trailing count")
	c.Eq(fmt.Sprintf("%d further event(s) skipped", skips-maxCaptureAnomalies), diag.Anomalies[len(diag.Anomalies)-1], "the trailing anomaly counts the rest")
	for _, a := range diag.Anomalies {
		c.NotStrContains(a, "stopped consuming the stream", "consumption is never cut off by a skip count")
	}
	var stored struct {
		StopReason string            `json:"stop_reason"`
		Usage      map[string]int64  `json:"usage"`
		Content    []json.RawMessage `json:"content"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &stored), "canonical unmarshal")
	c.Eq("end_turn", stored.StopReason, "stored stop reason")
	c.Eq(int64(99), stored.Usage["output_tokens"], "stored output tokens")
	c.Require().Len(stored.Content, 1, "the text block that accumulated")
}

// assertCanonicalJSON compares two JSON documents as values, never as bytes:
// key order and whitespace carry no meaning.
func assertCanonicalJSON(t *testing.T, want, got json.RawMessage, what string) {
	t.Helper()
	c := assert.NewAborting(t)
	var w, g any
	c.NoError(json.Unmarshal(want, &w), "%s: decode want", what)
	c.NoError(json.Unmarshal(got, &g), "%s: decode got", what)
	c.EqDeep(w, g, what)
}
