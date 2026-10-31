package responses_test

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// modeCase is one codec configuration the corpus is measured against: the golden
// the mode was recorded into, and the encode options that produced it.
type modeCase struct {
	name   string
	codec  responses.Codec
	stream bool
	golden string
}

var modes = []modeCase{
	{name: "api", codec: responses.Codec{Mode: responses.ModeAPI}, golden: ".responses-api.json"},
	{name: "chatgpt", codec: responses.Codec{Mode: responses.ModeChatGPT}, stream: true, golden: ".responses-chatgpt.json"},
}

// TestGoldenEncodeResponses pins every corpus case against the body today's
// builder produced for it, in both modes.
func TestGoldenEncodeResponses(t *testing.T) {
	for _, tc := range msgtest.Corpus() {
		for _, m := range modes {
			t.Run(tc.Name+"/"+m.name, func(t *testing.T) {
				ck := assert.NewAborting(t)
				body, err := m.codec.Encode(tc.Req, encodeOptions(tc, m.stream))
				ck.NoError(err, "encode %s", tc.Name)
				compareEncoded(t, tc, msgtest.Golden(filepath.Join("golden", tc.Name+m.golden)), body)
			})
		}
	}
}

// TestEncodeDeterministic pins that two encodes of the same request are the same
// bytes. The body is a struct, but opts.Extra is a map, so this is the test that
// catches a merge whose key order leaks into the output.
func TestEncodeDeterministic(t *testing.T) {
	for _, tc := range msgtest.Corpus() {
		for _, m := range modes {
			t.Run(tc.Name+"/"+m.name, func(t *testing.T) {
				ck := assert.NewAborting(t)
				first, err := m.codec.Encode(tc.Req, encodeOptions(tc, m.stream))
				ck.NoError(err, "first encode")
				second, err := m.codec.Encode(tc.Req, encodeOptions(tc, m.stream))
				ck.NoError(err, "second encode")
				ck.Eq(string(first), string(second), "two encodes of the same request differ")
			})
		}
	}
}

// TestEncodeResendsOnlyOwnMode pins the design's cross-provider rule one level
// below the family split: the encrypted blob is bound to the backend that
// produced it, so a codec drops the other Responses mode's reasoning item instead
// of replaying a blob its backend cannot verify.
func TestEncodeResendsOnlyOwnMode(t *testing.T) {
	tc := corpusCase(t, "thinking-foreign")
	api := encodedObject(t, responses.Codec{Mode: responses.ModeAPI}, tc, false)
	chatGPT := encodedObject(t, responses.Codec{Mode: responses.ModeChatGPT}, tc, true)
	ck := assert.NewAborting(t)
	ck.EqDeep([]string{"rs_1"}, reasoningIDs(t, api), "api mode reasoning items")
	ck.EqDeep([]string{"rs_2"}, reasoningIDs(t, chatGPT), "chatgpt mode reasoning items")
}

// TestEncodeDeveloperMessage pins the alternative system-prompt placement: the
// first input item, as a developer message, with no top-level instructions field.
func TestEncodeDeveloperMessage(t *testing.T) {
	ck := assert.NewAborting(t)
	body := encodedObject(t, responses.Codec{Mode: responses.ModeChatGPT, DevMessage: true}, corpusCase(t, "plain"), true)
	ck.NotHasKey(body, "instructions", "a developer message replaces the instructions field")
	items, ok := body["input"].([]any)
	ck.True(ok, "input is not an array")
	ck.Len(items, 2, "developer item plus the user message")
	first, ok := items[0].(map[string]any)
	ck.True(ok, "first input item is not an object")
	ck.Eq("developer", first["role"], "system prompt role")
	ck.Eq("message", first["type"], "system prompt item type")
	ck.EqDeep[any]([]any{map[string]any{"type": "input_text", "text": "You are terse."}}, first["content"], "system prompt content")
}

// TestUnknownModeIsRefused pins that a codec built without a mode fails loudly.
// The zero Mode is not a default: it parses no signature, so it would otherwise
// encode like the API mode while dropping every reasoning item in the transcript.
func TestUnknownModeIsRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	codec := responses.Codec{}
	_, err := codec.Encode(corpusCase(t, "plain").Req, msg.EncodeOptions{})
	ck.Error(err, "encode with no mode")
	_, err = codec.DecodeResponse([]byte(`{"id":"resp_1"}`))
	ck.Error(err, "decode with no mode")
	d := codec.NewStreamDecoder(io.NopCloser(strings.NewReader("")), time.Minute, 0)
	defer func() { ck.NoError(d.Close(), "close decoder") }()
	_, err = d.Next()
	ck.Error(err, "stream with no mode")
}

// TestDecodeResponse pins the non-streamed translation: block kinds, the stop
// reason, and the signature a reasoning item is given.
func TestDecodeResponse(t *testing.T) {
	ck := assert.NewAborting(t)
	body := `{
		"id": "resp_1",
		"model": "test-model",
		"status": "completed",
		"output": [
			{"type": "reasoning", "id": "rs_1", "encrypted_content": "blob", "summary": [{"type": "summary_text", "text": "hmm"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi "}, {"type": "output_text", "text": "there"}]},
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\": \"a\"}"}
		]
	}`
	resp, err := responses.Codec{Mode: responses.ModeChatGPT}.DecodeResponse([]byte(body))
	ck.NoError(err, "decode")
	ck.Eq("resp_1", resp.ID, "id")
	ck.Eq("test-model", resp.Model, "model")
	ck.Eq(msg.StopToolUse, resp.StopReason, "a function call stops the turn for tools")
	ck.EqDeep([]msg.Block{
		msg.Thinking{
			Text: "hmm",
			Sig:  msg.Signature{Dialect: msg.DialectResponsesChatGPT, Value: "openai-responses:chatgpt:rs_1:blob"},
		},
		msg.Text{Text: "hi there"},
		msg.ToolUse{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
	}, resp.Content, "content")
}

// TestDecodeResponseUsageByPresence pins the by-presence usage rules: a count the
// wire did not report stays nil, a reported zero does not.
func TestDecodeResponseUsageByPresence(t *testing.T) {
	cases := []struct {
		name string
		body string
		want msg.Usage
	}{
		{
			name: "all reported",
			body: `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":7},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":3}}}`,
			want: msg.Usage{
				Input:      msg.Ptr(60),
				Output:     msg.Ptr(9),
				CacheRead:  msg.Ptr(40),
				CacheWrite: msg.Ptr(7),
				Reasoning:  msg.Ptr(3),
			},
		},
		{
			name: "no details",
			body: `{"usage":{"input_tokens":12,"output_tokens":2}}`,
			want: msg.Usage{Input: msg.Ptr(12), Output: msg.Ptr(2)},
		},
		{
			name: "nothing reported",
			body: `{}`,
			want: msg.Usage{},
		},
		{
			name: "reported zeros stay reported",
			body: `{"usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0}}`,
			want: msg.Usage{Input: msg.Ptr(5), Output: msg.Ptr(0), CacheRead: msg.Ptr(0), CacheWrite: msg.Ptr(0)},
		},
		{
			name: "cache larger than the input count",
			body: `{"usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":9}}}`,
			want: msg.Usage{Input: msg.Ptr(0), CacheRead: msg.Ptr(9)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			resp, err := responses.Codec{Mode: responses.ModeAPI}.DecodeResponse([]byte(tc.body))
			ck.NoError(err, "decode")
			ck.EqDeep(tc.want, resp.Usage, "usage")
		})
	}
}

// encodeOptions is the option set a golden was recorded with: sess-1 as the
// session id, and the case's own top-level extras.
func encodeOptions(tc msgtest.Case, stream bool) msg.EncodeOptions {
	return msg.EncodeOptions{Stream: stream, SessionID: "sess-1", Extra: tc.Extra}
}

// compareEncoded compares a built body with its golden canonically. The recording
// builder ignored the case's extras, so they are lifted out of the built body and
// checked on their own — same keys and values, top-level key order free, which is
// the intended difference for provider extras.
func compareEncoded(t *testing.T, tc msgtest.Case, want, got []byte) {
	t.Helper()
	ck := assert.NewAborting(t)
	var wantDoc any
	ck.NoError(json.Unmarshal(want, &wantDoc), "golden is not JSON")
	var gotDoc map[string]any
	ck.NoError(json.Unmarshal(got, &gotDoc), "built body is not a JSON object")
	for k, v := range tc.Extra {
		ck.EqDeep(v, gotDoc[k], "extra field %q", k)
		delete(gotDoc, k)
	}
	ck.EqDeep[any](wantDoc, gotDoc, "built body does not match its golden")
}

// corpusCase returns the corpus case named name.
func corpusCase(t *testing.T, name string) msgtest.Case {
	t.Helper()
	for _, tc := range msgtest.Corpus() {
		if tc.Name == name {
			return tc
		}
	}
	t.Fatalf("corpus case %q not found", name)
	return msgtest.Case{}
}

// encodedObject encodes tc and decodes the body into a generic object.
func encodedObject(t *testing.T, c responses.Codec, tc msgtest.Case, stream bool) map[string]any {
	t.Helper()
	ck := assert.NewAborting(t)
	body, err := c.Encode(tc.Req, encodeOptions(tc, stream))
	ck.NoError(err, "encode %s", tc.Name)
	var obj map[string]any
	ck.NoError(json.Unmarshal(body, &obj), "decode built body")
	return obj
}

// reasoningIDs lists the reasoning item ids a body's input carries, in order.
func reasoningIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	ck := assert.NewAborting(t)
	items, ok := body["input"].([]any)
	ck.True(ok, "input is not an array")
	ids := []string{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	return ids
}
