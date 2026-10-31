package openaichat_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
	"go.graveland.dev/rafiki/pkg/msg/openaichat"
)

// TestGoldenEncodeOpenAIChat is the request oracle: every corpus case encodes to
// the body the previous sender produced, in both stream variants.
func TestGoldenEncodeOpenAIChat(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range msgtest.Corpus() {
		for _, stream := range []bool{false, true} {
			name := "golden/" + tc.Name + ".openaichat.json"
			if stream {
				name = "golden/" + tc.Name + ".openaichat-stream.json"
			}
			got, err := openaichat.Encode(tc.Req, msg.EncodeOptions{Stream: stream, Extra: tc.Extra})
			c.NoError(err, "encode %s (stream %v)", tc.Name, stream)

			fields := canonicalObject(c, got, "encoded "+tc.Name)
			want := canonicalObject(c, msgtest.Golden(name), name)
			// Intended difference 2: Encode merges opts.Extra, which the recorded
			// bodies for this dialect predate. Every other key must match exactly,
			// and each merged key must carry the value Extra gave it.
			for k, v := range tc.Extra {
				c.EqDeep(v, fields[k], "case %s: merged extra %q", tc.Name, k)
				delete(fields, k)
			}
			c.EqDeep(want, fields, "case %s (stream %v)", tc.Name, stream)
		}
	}
}

// TestEncodeDeterministic pins that two encodes of one request are the same
// bytes, so a body can be compared (and cached) byte-wise.
func TestEncodeDeterministic(t *testing.T) {
	c := assert.NewAborting(t)
	req := msgtest.Corpus()[1].Req

	first, err := openaichat.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode once")
	second, err := openaichat.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode twice")

	c.True(bytes.Equal(first, second), "two encodes of one request differ:\n%s\n%s", first, second)
}

// TestEncodeDropsThinkingAndOpaque pins the foreign-block rule: a thinking block
// is never replayed to this dialect, an Opaque block is never guessed at, and a
// block's Ext residue is ignored entirely.
func TestEncodeDropsThinkingAndOpaque(t *testing.T) {
	c := assert.NewAborting(t)

	req := &msg.Request{
		Model:     "test-model",
		MaxTokens: 64,
		Messages: []msg.Message{
			{Role: msg.Assistant, Content: []msg.Block{
				msg.Thinking{Text: "hmm", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}},
				msg.Opaque{Provider: msg.DialectOpenAIChat, Raw: json.RawMessage(`{"type":"audio","id":"aud_1"}`)},
				msg.Text{Text: "keep"},
			}},
			{Role: msg.User, Content: []msg.Block{
				msg.Text{Text: "go", Ext: msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"citations":[{"type":"char_location"}]}`}},
			}},
			{Role: msg.Assistant, Content: []msg.Block{
				msg.ToolUse{
					ID:    "call_1",
					Name:  "read_file",
					Input: json.RawMessage(`{"path":"a"}`),
					Ext:   msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"caller":{"type":"direct"}}`},
				},
			}},
		},
	}

	body, err := openaichat.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode")

	fields := canonicalObject(c, body, "encoded body")
	messages := arrayAt(c, fields["messages"], "messages")
	c.Eq(3, len(messages), "one message per translated turn")

	assistant := objectAt(c, messages[0], "messages[0]")
	c.Eq("assistant", assistant["role"])
	c.Eq("keep", assistant["content"], "thinking and opaque blocks are dropped, the text stays")
	c.NotHasKey(assistant, "tool_calls")

	user := objectAt(c, messages[1], "messages[1]")
	c.Eq("go", user["content"], "Ext residue is ignored")
	c.NotStrContains(mustJSON(c, user), "citations", "a foreign citations residue is never sent")

	caller := objectAt(c, messages[2], "messages[2]")
	c.NotHasKey(caller, "content", "an assistant turn carrying only a tool call has no content")
	calls := arrayAt(c, caller["tool_calls"], "tool_calls")
	c.Eq(1, len(calls), "the tool call survives")
	call := objectAt(c, calls[0], "tool_calls[0]")
	c.NotHasKey(call, "caller", "a foreign caller residue is never sent")
	c.EqDeep(map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      "read_file",
			"arguments": `{"path":"a"}`,
		},
	}, call)
}

// TestDecodeResponseKeepsUnmodelledFinishReason pins that a finish_reason this
// codec does not map is not erased: the enum stays unknown and the wire's own
// spelling rides on the response, so a record keeps what the provider said.
func TestDecodeResponseKeepsUnmodelledFinishReason(t *testing.T) {
	c := assert.NewAborting(t)
	const wire = "model_context_window_exceeded"
	resp, err := openaichat.DecodeResponse([]byte(`{"id":"c1","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"` + wire + `"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	c.NoError(err, "decode")
	c.Eq(msg.StopUnknown, resp.StopReason, "a value this codec does not map stays unknown")
	c.Eq(wire, resp.StopReasonRaw, "the wire's own spelling is kept")
	c.Eq(wire, resp.StopReasonWire(), "the record falls back to the wire's spelling")

	body, err := msganthropic.ResponseJSON(resp)
	c.NoError(err, "ResponseJSON")
	var stored struct {
		StopReason string `json:"stop_reason"`
	}
	c.NoError(json.Unmarshal(body, &stored), "unmarshal the stored response")
	c.Eq(wire, stored.StopReason, "the record keeps the stop reason the provider reported")
}

// TestDecodeGolden compares a decoded response with its recorded decode, apart
// from the intended differences: the cache-adjusted input, the reasoning count,
// and absence rather than zero for a count the wire did not report.
func TestDecodeGolden(t *testing.T) {
	c := assert.NewAborting(t)

	fixture := msgtest.Golden("responses/openaichat-tooluse.json")
	got, err := openaichat.DecodeResponse(fixture)
	c.NoError(err, "decode the tool-use fixture")

	golden := decodeGolden(c, "golden/openaichat-tooluse.json.decoded.json")
	c.Eq(golden.ID, got.ID)
	c.Eq(golden.Model, got.Model)
	c.Eq(golden.StopReason, got.StopReason.String())
	c.Eq(golden.Provider, got.Provider)
	c.EqDeep(golden.Content, renderedContent(c, got.Content), "content")

	var wire struct {
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	c.NoError(json.Unmarshal(fixture, &wire), "read the fixture usage")
	c.Eq(int64(100), wire.Usage.PromptTokens, "the fixture's prompt_tokens")

	// Intended differences 3, 4 and 5: prompt_tokens includes the cached prefix,
	// so Input is the uncached remainder and CacheRead carries what was cached;
	// Reasoning comes from completion_tokens_details.
	c.EqDeep(msg.Ptr(wire.Usage.PromptTokens-40), got.Usage.Input, "input is prompt_tokens net of the cached prefix")
	c.EqDeep(msg.Ptr(40), got.Usage.CacheRead)
	c.EqDeep(msg.Ptr(3), got.Usage.Reasoning)
	c.EqDeep(msg.Ptr(25), got.Usage.Output)
	c.True(got.Usage.CacheWrite == nil, "Chat Completions reports no cache write")
}

// TestDecodeNoUsageDetails pins the Ollama shape: no cache or reasoning detail
// means nil, not zero.
func TestDecodeNoUsageDetails(t *testing.T) {
	c := assert.NewAborting(t)

	got, err := openaichat.DecodeResponse(msgtest.Golden("responses/openaichat-nousage-details.json"))
	c.NoError(err, "decode the Ollama-shaped fixture")

	golden := decodeGolden(c, "golden/openaichat-nousage-details.json.decoded.json")
	c.Eq(golden.ID, got.ID)
	c.Eq(golden.Model, got.Model)
	c.Eq(golden.StopReason, got.StopReason.String())
	c.EqDeep(golden.Content, renderedContent(c, got.Content), "content")

	c.EqDeep(msg.Ptr(12), got.Usage.Input, "input is prompt_tokens when the wire reports no cached prefix")
	c.True(got.Usage.CacheRead == nil, "no cached_tokens reported means CacheRead is nil, not 0")
	c.EqDeep(msg.Ptr(3), got.Usage.Output)
	c.True(got.Usage.Reasoning == nil, "no reasoning_tokens reported means Reasoning is nil")
	c.True(got.Usage.CacheWrite == nil, "Chat Completions reports no cache write")
}

// TestStopReasonTable pins the finish_reason mapping. An unrecognised value is
// StopUnknown rather than a clean end of turn.
func TestStopReasonTable(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		finish string
		want   msg.StopReason
	}{
		{"stop", msg.StopEndTurn},
		{"length", msg.StopMaxTokens},
		{"tool_calls", msg.StopToolUse},
		{"function_call", msg.StopToolUse},
		{"content_filter", msg.StopRefusal},
		{"", msg.StopUnknown},
		{"eos", msg.StopUnknown},
	} {
		body := fmt.Sprintf(
			`{"id":"r1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":%q}]}`,
			tc.finish)
		got, err := openaichat.DecodeResponse([]byte(body))
		c.NoError(err, "decode finish_reason %q", tc.finish)
		c.Eq(tc.want, got.StopReason, "finish_reason %q", tc.finish)
	}
}

// TestClassify has one row per classification rule, in the order the rules are
// applied, so a reordering that changes an answer fails here.
func TestClassify(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   msg.ErrorKind
	}{
		{
			name:   "usage limit outranks the 429 it arrives on",
			status: 429,
			body:   `{"error":{"code":"insufficient_quota","type":"insufficient_quota","message":"You exceeded your current quota"}}`,
			want:   msg.ErrUsageLimit,
		},
		{
			name:   "usage limit reached, in-band",
			status: 0,
			body:   `{"error":{"type":"usage_limit_reached","message":"You have reached your usage limit"}}`,
			want:   msg.ErrUsageLimit,
		},
		{
			name:   "usage not included",
			status: 400,
			body:   `{"error":{"code":"usage_not_included","message":"usage not included in your plan"}}`,
			want:   msg.ErrUsageLimit,
		},
		{
			name:   "context length exceeded by code",
			status: 400,
			body:   `{"error":{"code":"context_length_exceeded","message":"too big"}}`,
			want:   msg.ErrPromptTooLarge,
		},
		{
			name:   "maximum context length in the message",
			status: 400,
			body:   `{"error":{"message":"This model's maximum context length is 8192 tokens."}}`,
			want:   msg.ErrPromptTooLarge,
		},
		{
			name:   "prompt is too long at 413",
			status: 413,
			body:   `{"error":{"message":"prompt is too long: 200000 tokens"}}`,
			want:   msg.ErrPromptTooLarge,
		},
		{
			name:   "no endpoints found at 404",
			status: 404,
			body:   `{"error":{"message":"No endpoints found for some/model"}}`,
			want:   msg.ErrNoEligible,
		},
		{
			name:   "no allowed providers at 400",
			status: 400,
			body:   `{"error":{"message":"No allowed providers are available for the selected model"}}`,
			want:   msg.ErrNoEligible,
		},
		{
			name:   "http 429",
			status: 429,
			body:   `{}`,
			want:   msg.ErrRateLimit,
		},
		{
			name:   "numeric payload code 429 in-band",
			status: 0,
			body:   `{"error":{"code":429,"message":"slow down"}}`,
			want:   msg.ErrRateLimit,
		},
		{
			name:   "numeric code as a string",
			status: 0,
			body:   `{"error":{"code":"429","message":"slow down"}}`,
			want:   msg.ErrRateLimit,
		},
		{
			name:   "rate_limit_exceeded type",
			status: 0,
			body:   `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			want:   msg.ErrRateLimit,
		},
		{
			name:   "overloaded_error outranks the server range",
			status: 529,
			body:   `{"error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   msg.ErrOverloaded,
		},
		{
			name:   "http 500",
			status: 500,
			body:   `{}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "numeric payload code 503 in-band",
			status: 0,
			body:   `{"error":{"code":503,"message":"unavailable"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "numeric code as a string, in the server range",
			status: 0,
			body:   `{"error":{"code":"502","message":"bad gateway"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "timeout_error",
			status: 0,
			body:   `{"error":{"type":"timeout_error","message":"timed out"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "api_error",
			status: 0,
			body:   `{"error":{"type":"api_error","message":"boom"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "server_error",
			status: 0,
			body:   `{"error":{"type":"server_error","message":"boom"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "terminal invalid request",
			status: 400,
			body:   `{"error":{"type":"invalid_request_error","message":"bad request"}}`,
			want:   msg.ErrOther,
		},
		{
			name:   "bare payload, not nested under error",
			status: 400,
			body:   `{"code":"insufficient_quota","type":"insufficient_quota","message":"quota"}`,
			want:   msg.ErrUsageLimit,
		},
		{
			name:   "unauthorized",
			status: 401,
			body:   `{"error":{"message":"invalid api key"}}`,
			want:   msg.ErrOther,
		},
		{
			name:   "unparseable body",
			status: 400,
			body:   `not json at all`,
			want:   msg.ErrOther,
		},
	} {
		c.Eq(tc.want, openaichat.Classify(tc.status, nil, []byte(tc.body)), "classify %q", tc.name)
	}
}

// TestClassifyHTTPAndInBandParity pins that the payload rules apply at status 0
// exactly as they do at the HTTP status they usually arrive with — the in-band
// error of a stream must classify like the same refusal over HTTP.
func TestClassifyHTTPAndInBandParity(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   msg.ErrorKind
	}{
		{
			name:   "rate limit",
			status: 429,
			body:   `{"error":{"code":429,"type":"rate_limit_exceeded","message":"slow down"}}`,
			want:   msg.ErrRateLimit,
		},
		{
			name:   "overload",
			status: 529,
			body:   `{"error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   msg.ErrOverloaded,
		},
		{
			name:   "server error",
			status: 500,
			body:   `{"error":{"code":500,"type":"server_error","message":"boom"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "timeout",
			status: 504,
			body:   `{"error":{"type":"timeout_error","message":"timed out"}}`,
			want:   msg.ErrTransient,
		},
		{
			name:   "context length",
			status: 400,
			body:   `{"error":{"code":"context_length_exceeded","message":"too big"}}`,
			want:   msg.ErrPromptTooLarge,
		},
		{
			name:   "no eligible provider",
			status: 404,
			body:   `{"error":{"message":"No endpoints found for some/model"}}`,
			want:   msg.ErrNoEligible,
		},
		{
			name:   "usage limit",
			status: 429,
			body:   `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`,
			want:   msg.ErrUsageLimit,
		},
		{
			name:   "terminal invalid request",
			status: 400,
			body:   `{"error":{"type":"invalid_request_error","message":"bad request"}}`,
			want:   msg.ErrOther,
		},
	} {
		http := openaichat.Classify(tc.status, nil, []byte(tc.body))
		inBand := openaichat.Classify(0, nil, []byte(tc.body))
		c.Eq(tc.want, http, "%s: over HTTP", tc.name)
		c.Eq(tc.want, inBand, "%s: in band", tc.name)
	}
}

// decodedGolden is the recorded decode of one response fixture, as the corpus
// writes it: provider-independent fields plus content in the stored block shape.
type decodedGolden struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	StopReason string      `json:"stop_reason"`
	Provider   string      `json:"provider"`
	Usage      goldenUsage `json:"usage"`
	Content    []any       `json:"content"`
}

type goldenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func decodeGolden(c *assert.C, name string) decodedGolden {
	var g decodedGolden
	c.NoError(json.Unmarshal(msgtest.Golden(name), &g), "decode golden %s", name)
	return g
}

// renderedContent renders decoded blocks the way the corpus records content, so
// a decode can be compared with its golden without a second encoder here.
func renderedContent(c *assert.C, blocks []msg.Block) []any {
	enc, err := msganthropic.EncodeContent(msg.Message{Role: msg.Assistant, Content: blocks})
	c.NoError(err, "render decoded content")
	var out []any
	c.NoError(json.Unmarshal(enc, &out), "read rendered content")
	return out
}

func canonicalObject(c *assert.C, b []byte, what string) map[string]any {
	var v map[string]any
	c.NoError(json.Unmarshal(b, &v), "decode %s as a JSON object", what)
	return v
}

func objectAt(c *assert.C, v any, what string) map[string]any {
	m, ok := v.(map[string]any)
	c.True(ok, "%s is a JSON object, got %T", what, v)
	return m
}

func arrayAt(c *assert.C, v any, what string) []any {
	a, ok := v.([]any)
	c.True(ok, "%s is a JSON array, got %T", what, v)
	return a
}

func mustJSON(c *assert.C, v any) string {
	b, err := json.Marshal(v)
	c.NoError(err, "marshal for inspection")
	return string(b)
}
