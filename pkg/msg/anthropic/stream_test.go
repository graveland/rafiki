package anthropic_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
)

// TestDecodeGolden decodes each recorded whole-body fixture and compares it
// with the corpus's decoded golden. Usage is compared by value rather than by
// presence: the golden records the counts the wire carried, and a field the
// wire did not carry is exactly what intended difference 5 keeps nil.
func TestDecodeGolden(t *testing.T) {
	for _, name := range anthropicFixtures {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			resp, err := anthropic.DecodeResponse(anthropicFixture(t, name+".json"))
			c.NoError(err, "decode %s", name)
			assertMatchesGolden(t, resp, name+".json")
		})
	}
}

// TestStreamGolden drives each recorded SSE fixture through the decoder and the
// accumulator and compares the result with the same decoded golden its
// whole-body twin produces: a streamed response must mean what the recorded one
// means, block for block.
func TestStreamGolden(t *testing.T) {
	for _, name := range anthropicFixtures {
		t.Run(name, func(t *testing.T) {
			resp, _ := streamResponse(t, anthropicFixture(t, name+".sse"))
			assertMatchesGolden(t, resp, name+".sse")
		})
	}
}

// TestStreamMatchesDecodeResponseWithExt pins that the two decode paths agree
// on the blocks they build, residue included: a tool_use's caller, a text
// block's start citations and the citations deltas add, and a thinking block's
// initial signature all have to survive streaming exactly as they survive a
// whole body.
func TestStreamMatchesDecodeResponseWithExt(t *testing.T) {
	for _, name := range anthropicFixtures {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			whole, err := anthropic.DecodeResponse(anthropicFixture(t, name+".json"))
			c.NoError(err, "decode %s.json", name)
			streamed, _ := streamResponse(t, anthropicFixture(t, name+".sse"))
			compareCanonical(t, contentJSON(t, whole), contentJSON(t, streamed))
		})
	}

	t.Run("initial-signature-and-added-citations", func(t *testing.T) {
		c := assert.NewAborting(t)
		whole, err := anthropic.DecodeResponse([]byte(extJSON))
		c.NoError(err, "decode the whole body")
		streamed, _ := streamResponse(t, []byte(extSSE))
		compareCanonical(t, contentJSON(t, whole), contentJSON(t, streamed))
	})
}

// TestDecodeUsagePresence pins the usage rule: a count absent from the body
// stays nil, and a reported zero stays a reported zero.
func TestDecodeUsagePresence(t *testing.T) {
	c := assert.NewAborting(t)
	resp, err := anthropic.DecodeResponse([]byte(`{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0}}`))
	c.NoError(err, "decode")
	c.Eq(int64(10), msg.Val(resp.Usage.Input), "input tokens")
	c.Eq(int64(5), msg.Val(resp.Usage.Output), "output tokens")
	c.True(resp.Usage.CacheRead == nil, "an absent cache_read must stay nil, not 0")
	c.True(resp.Usage.CacheWrite != nil, "a reported zero cache_write must be present")
	c.Eq(int64(0), msg.Val(resp.Usage.CacheWrite), "cache write tokens")
	c.True(resp.Usage.Reasoning == nil, "Anthropic reports no reasoning tokens")

	nulled, err := anthropic.DecodeResponse([]byte(`{"id":"msg_1","usage":{"input_tokens":null}}`))
	c.NoError(err, "decode a null count")
	c.True(nulled.Usage.Input == nil, "a null count is not a report of zero")
}

// TestDecodeStoredMessageAPIUnchanged pins that the new response decoder and
// the stored-message decoder agree: they are two entry points to one block
// decoder, so a row read from storage and a response read from the wire must
// produce the same blocks.
func TestDecodeStoredMessageAPIUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	body := anthropicFixture(t, "anthropic-tooluse.json")
	var wire struct {
		Content json.RawMessage `json:"content"`
	}
	c.NoError(json.Unmarshal(body, &wire), "read the fixture's content")
	stored, err := anthropic.Decode("assistant", wire.Content)
	c.NoError(err, "decode stored content")
	resp, err := anthropic.DecodeResponse(body)
	c.NoError(err, "decode response")
	c.EqDeep(stored, msg.Message{Role: msg.Assistant, Content: resp.Content})
}

// TestStreamProviderPreserved pins that OpenRouter's served-by field survives
// the stream: it is reported once, on message_start.
func TestStreamProviderPreserved(t *testing.T) {
	c := assert.NewAborting(t)
	resp, _ := streamResponse(t, []byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","provider":"Together","usage":{"input_tokens":3}}}

event: message_stop
data: {"type":"message_stop"}

`))
	c.Eq("Together", resp.Provider)
}

// TestStreamProviderAbsentStaysEmpty pins the other half: a native Anthropic
// stream names no provider, and the response must not invent one.
func TestStreamProviderAbsentStaysEmpty(t *testing.T) {
	c := assert.NewAborting(t)
	resp, _ := streamResponse(t, []byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":3}}}

event: message_stop
data: {"type":"message_stop"}

`))
	c.Eq("", resp.Provider)
}

// TestStreamInBandErrorIsAPIError pins that an SSE error event becomes the same
// APIError a status-code refusal becomes, classified from the event's own
// payload: a rate limit arriving mid-stream is still a rate limit.
func TestStreamInBandErrorIsAPIError(t *testing.T) {
	c := assert.NewAborting(t)
	dec := anthropic.NewStreamDecoder(io.NopCloser(strings.NewReader(`event: error
data: {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}

`)))
	_, err := dec.Next()
	c.Error(err, "an error event must not be swallowed")
	apiErr := c.ErrorAs[*msg.APIError](err, "the in-band error must be an APIError")
	c.Eq(msg.ErrRateLimit, apiErr.Kind, "an in-band rate limit is a rate limit")
	c.Eq(0, apiErr.Status, "an in-band error has no HTTP status")
	c.Eq(msg.DialectAnthropic, apiErr.Dialect)
	c.Eq("slow down", apiErr.Message)
	c.NotEmpty(apiErr.Body, "the event's JSON is the error body")
	c.NoError(dec.Close(), "close")
}

// TestStreamTruncatedIsUnexpectedEOF pins that a stream cut before its
// message_stop is a failure, not a short answer: a caller must not store a
// half-generated turn as if the model had finished.
func TestStreamTruncatedIsUnexpectedEOF(t *testing.T) {
	c := assert.NewAborting(t)
	body := anthropicFixture(t, "anthropic-tooluse.sse")
	cut := strings.Index(string(body), "event: message_stop")
	c.True(cut > 0, "the fixture must carry a message_stop")
	dec := anthropic.NewStreamDecoder(io.NopCloser(bytes.NewReader(body[:cut])))
	for {
		_, err := dec.Next()
		if err == nil {
			continue
		}
		c.True(errors.Is(err, io.ErrUnexpectedEOF), "a truncated stream must end in io.ErrUnexpectedEOF, got %v", err)
		break
	}
	c.NoError(dec.Close(), "close")
}

// TestStreamServerToolStaysOpaque pins that a server-tool block is never
// mistranslated into a client tool call: its fragments are folded into the
// block's own JSON, and its type and every key the core does not model survive.
func TestStreamServerToolStaysOpaque(t *testing.T) {
	c := assert.NewAborting(t)
	resp, _ := streamResponse(t, anthropicFixture(t, "anthropic-servertool.sse"))
	c.Len(resp.Content, 2, "the fixture carries two blocks")

	use, ok := resp.Content[0].(msg.Opaque)
	c.True(ok, "a server_tool_use must stay opaque, got %T", resp.Content[0])
	c.Eq("server_tool_use", msg.OpaqueType(use))
	raw := rawObject(t, use.Raw)
	c.Eq(`{"query":"rafiki"}`, string(raw["input"]), "the fragmented input must be folded in")
	c.HasKey(raw, "provider_metadata", "start residue must survive")

	result, ok := resp.Content[1].(msg.Opaque)
	c.True(ok, "a server-tool result must stay opaque, got %T", resp.Content[1])
	c.Eq("web_search_tool_result", msg.OpaqueType(result))

	for i, b := range resp.Content {
		_, isToolUse := b.(msg.ToolUse)
		c.False(isToolUse, "block %d must not become a client tool call", i)
	}
}

// TestStreamUnknownContentDeltaErrors pins that a delta this codec does not
// model is an error rather than a silent gap in the model's output.
func TestStreamUnknownContentDeltaErrors(t *testing.T) {
	c := assert.NewAborting(t)
	dec := anthropic.NewStreamDecoder(io.NopCloser(strings.NewReader(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"mystery_delta","value":"x"}}

`)))
	_, err := dec.Next()
	c.NoError(err, "the block start is fine")
	_, err = dec.Next()
	c.ErrorContains(err, "unsupported content block delta", "an unmodelled delta must be reported")
	c.NoError(dec.Close(), "close")
}

// TestStreamMalformedToolInput pins the repair policy split: a client tool
// call whose arguments did not parse leaves the accumulator to substitute an
// empty object while the raw fragments stay available, and a server-tool block
// keeps its own type either way.
func TestStreamMalformedToolInput(t *testing.T) {
	c := assert.NewAborting(t)

	client, dec := streamResponse(t, []byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`))
	use, ok := client.Content[0].(msg.ToolUse)
	c.True(ok, "a client tool call stays typed, got %T", client.Content[0])
	c.Eq("{}", string(use.Input), "the accumulator's repair substitutes an empty object")
	raw, ok := dec.InvalidInput(0)
	c.True(ok, "the unparseable fragments must be retained")
	c.Eq(`{"path":`, string(raw))

	server, _ := streamResponse(t, []byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{oops"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`))
	opaque, ok := server.Content[0].(msg.Opaque)
	c.True(ok, "a server-tool block stays opaque, got %T", server.Content[0])
	c.Eq("server_tool_use", msg.OpaqueType(opaque), "the server-tool type must be preserved")
	c.Eq(`{}`, string(rawObject(t, opaque.Raw)["input"]), "a repaired server-tool input is an empty object")
}

// anthropicFixtures are the recorded Anthropic responses, each with a
// whole-body and a streamed form.
var anthropicFixtures = []string{
	"anthropic-tooluse",
	"anthropic-citations",
	"anthropic-caller",
	"anthropic-servertool",
}

// anthropicFixture reads one recorded response.
func anthropicFixture(t *testing.T, name string) []byte {
	t.Helper()
	return msgtest.Golden(filepath.Join("responses", name))
}

// streamResponse decodes a streamed fixture through the decoder and the
// accumulator, which is what a sender does with it.
func streamResponse(t *testing.T, body []byte) (*msg.Response, *anthropic.Decoder) {
	t.Helper()
	c := assert.NewAborting(t)
	dec := anthropic.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)))
	concrete, ok := dec.(*anthropic.Decoder)
	c.True(ok, "NewStreamDecoder must return this package's decoder")
	acc := msg.NewAccumulator()
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		c.NoError(err, "next event")
		c.NoError(acc.Add(ev), "accumulate %T", ev)
	}
	c.NoError(dec.Close(), "close the stream")
	return acc.Response(), concrete
}

// assertMatchesGolden compares a decoded response with a decoded golden.
func assertMatchesGolden(t *testing.T, resp *msg.Response, name string) {
	t.Helper()
	c := assert.NewAborting(t)
	var want struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Provider   string `json:"provider"`
		Usage      struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	}
	c.NoError(json.Unmarshal(msgtest.Golden(filepath.Join("golden", name+".decoded.json")), &want), "read the %s golden", name)
	c.Eq(want.ID, resp.ID, "%s: id", name)
	c.Eq(want.Model, resp.Model, "%s: model", name)
	c.Eq(want.StopReason, resp.StopReason.String(), "%s: stop reason", name)
	c.Eq(want.Provider, resp.Provider, "%s: provider", name)
	c.Eq(want.Usage.Input, msg.Val(resp.Usage.Input), "%s: input tokens", name)
	c.Eq(want.Usage.Output, msg.Val(resp.Usage.Output), "%s: output tokens", name)
	c.Eq(want.Usage.CacheRead, msg.Val(resp.Usage.CacheRead), "%s: cache read tokens", name)
	c.Eq(want.Usage.CacheWrite, msg.Val(resp.Usage.CacheWrite), "%s: cache write tokens", name)
	compareCanonical(t, want.Content, contentJSON(t, resp))
}

// contentJSON renders a response's blocks the way the decoded goldens do.
func contentJSON(t *testing.T, resp *msg.Response) []byte {
	t.Helper()
	c := assert.NewAborting(t)
	out, err := anthropic.EncodeContent(msg.Message{Role: msg.Assistant, Content: resp.Content})
	c.NoError(err, "encode content")
	return out
}

// rawObject reads a block's raw JSON as a keyed object.
func rawObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	c := assert.NewAborting(t)
	var obj map[string]json.RawMessage
	c.NoError(json.Unmarshal(raw, &obj), "read block JSON")
	return obj
}

// extJSON is the whole-body form of a response carrying a start citation, a
// citation added by a delta, and a signature seeded at the block start and
// extended by a delta.
const extJSON = `{
  "id": "msg_ext",
  "model": "m",
  "stop_reason": "end_turn",
  "provider": "P",
  "usage": {"input_tokens": 1, "output_tokens": 2},
  "content": [
    {"type": "text", "text": "the claim", "citations": [
      {"type": "char_location", "cited_text": "start"},
      {"type": "char_location", "cited_text": "added"}
    ]},
    {"type": "thinking", "thinking": "deep", "signature": "sig-0sig-1"}
  ]
}`

// extSSE is extJSON as a stream.
const extSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_ext","model":"m","provider":"P","usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","citations":[{"type":"char_location","cited_text":"start"}]}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the claim"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"added"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":"sig-0"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"deep"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
