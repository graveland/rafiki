package responses_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// streamFixture is one recorded Responses stream and the mode that produced it.
type streamFixture struct {
	name string
	mode responses.Mode
	// reasoning is what the wire reported in output_tokens_details.reasoning_tokens.
	// The decoded golden cannot express it (it decodes Reasoning, which the golden
	// shape has no field for), so it is pinned here instead.
	reasoning int64
}

var streamFixtures = []streamFixture{
	{name: "responses-turn1.response.sse", mode: responses.ModeChatGPT},
	{name: "responses-turn2.response.sse", mode: responses.ModeChatGPT},
	{name: "responses-std_turn1.response.sse", mode: responses.ModeAPI, reasoning: 14},
	{name: "responses-std_turn2.response.sse", mode: responses.ModeAPI},
}

// TestStreamGolden pins each recorded stream, accumulated into a native response,
// against the golden the SDK-based decoder recorded. The content is compared as
// stored-message JSON — the native model's own canonical form — so a block kind,
// its order and its fields all have to match.
func TestStreamGolden(t *testing.T) {
	for _, fx := range streamFixtures {
		t.Run(fx.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			resp := accumulate(t, fx.mode, msgtest.Golden(filepath.Join("responses", fx.name)))

			stored, err := msganthropic.EncodeContent(msg.Message{Role: msg.Assistant, Content: resp.Content})
			ck.NoError(err, "encode content")
			var content any
			ck.NoError(json.Unmarshal(stored, &content), "decode encoded content")

			var want any
			ck.NoError(json.Unmarshal(msgtest.Golden(filepath.Join("golden", fx.name+".decoded.json")), &want), "decode golden")

			got := map[string]any{
				"id":          resp.ID,
				"model":       resp.Model,
				"stop_reason": resp.StopReason.String(),
				"provider":    resp.Provider,
				"usage": map[string]any{
					"input":       msg.Val(resp.Usage.Input),
					"output":      msg.Val(resp.Usage.Output),
					"cache_read":  msg.Val(resp.Usage.CacheRead),
					"cache_write": msg.Val(resp.Usage.CacheWrite),
				},
				"content": content,
			}
			// Both sides go through JSON, so the golden's numbers and this
			// response's numbers compare as the same kind of value.
			encoded, err := json.Marshal(got)
			ck.NoError(err, "marshal decoded response")
			var gotDoc any
			ck.NoError(json.Unmarshal(encoded, &gotDoc), "normalise decoded response")
			ck.EqDeep[any](want, gotDoc, "decoded stream does not match its golden")

			ck.NotZero(resp.Usage.Reasoning, "the wire reported reasoning tokens")
			ck.Eq(fx.reasoning, msg.Val(resp.Usage.Reasoning), "reasoning tokens")
		})
	}
}

// TestStreamUsageLimitIsUsageLimitError pins that a subscription usage limit is
// terminal and keeps its own type: an APIError with a 429-ish kind would be
// retried through a backoff that resets tomorrow.
func TestStreamUsageLimitIsUsageLimitError(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		code string
		plan string
	}{
		{
			name: "nested error object",
			body: sse(`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
				`{"type":"error","error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"out of budget","plan_type":"plus","resets_at":1893456000}}`),
			code: "usage_limit_reached",
			plan: "plus",
		},
		{
			name: "flat error event",
			body: sse(`{"type":"error","code":"insufficient_quota","message":"out of budget"}`),
			code: "insufficient_quota",
		},
		{
			name: "response.failed",
			body: sse(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"usage_not_included","message":"no plan"}}}`),
			code: "usage_not_included",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			err := drainErr(t, responses.ModeChatGPT, tc.body)
			var ue *responses.UsageLimitError
			ck.True(errors.As(err, &ue), "a usage limit is a *UsageLimitError, got %T: %v", err, err)
			ck.Eq(tc.code, ue.Code, "code")
			ck.Eq(tc.plan, ue.PlanType, "plan type")
			var apiErr *msg.APIError
			ck.False(errors.As(err, &apiErr), "a usage limit is never an APIError")
		})
	}
}

// TestStreamContextLengthIsPromptTooLarge pins that an over-long prompt arrives
// as ErrPromptTooLarge, which is what the trim path keys on, whether the endpoint
// reported it in an error event or inside a failed response.
func TestStreamContextLengthIsPromptTooLarge(t *testing.T) {
	cases := []struct {
		name string
		mode responses.Mode
		body []byte
	}{
		{
			name: "error event",
			mode: responses.ModeAPI,
			body: sse(`{"type":"error","code":"context_length_exceeded","message":"prompt is too long"}`),
		},
		{
			name: "failed response",
			mode: responses.ModeChatGPT,
			body: sse(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"too big"}}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			err := drainErr(t, tc.mode, tc.body)
			var apiErr *msg.APIError
			ck.True(errors.As(err, &apiErr), "an over-long prompt is an APIError, got %T: %v", err, err)
			ck.Eq(msg.ErrPromptTooLarge, apiErr.Kind, "kind")
			ck.Eq(tc.mode.Dialect(), apiErr.Dialect, "dialect")
			ck.Zero(apiErr.Status, "an in-band error carries no status")
			ck.NotEmpty(apiErr.Message, "the endpoint's own message is carried")
			ck.NotEmpty(apiErr.Body, "the in-band event's JSON is carried")
		})
	}
}

// TestStreamRateLimitIsRateLimit pins that an in-band rate limit reaches the
// caller as ErrRateLimit. That kind is what routes the send to the sender's
// rate-limit loop (backpressure across every send for the model) instead of a
// generic per-turn retry or a terminal failure.
func TestStreamRateLimitIsRateLimit(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded"}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			ck := assert.NewAborting(t)
			err := drainErr(t, responses.ModeAPI, sse(payload))
			var apiErr *msg.APIError
			ck.True(errors.As(err, &apiErr), "a rate limit is an APIError, got %T: %v", err, err)
			ck.Eq(msg.ErrRateLimit, apiErr.Kind, "kind")
			var ue *responses.UsageLimitError
			ck.False(errors.As(err, &ue), "a rate limit is not a usage limit")
		})
	}
}

// TestStreamIdleTimeout pins the watchdog: a backend that goes silent is a
// retryable condition, reported as a transient APIError rather than a hang.
func TestStreamIdleTimeout(t *testing.T) {
	ck := assert.NewAborting(t)
	reader, writer := io.Pipe()
	defer func() { ck.NoError(writer.Close(), "close writer") }()
	d := responses.Codec{Mode: responses.ModeAPI}.NewStreamDecoder(reader, 20*time.Millisecond, 0)
	defer func() { ck.NoError(d.Close(), "close decoder") }()

	_, err := d.Next()
	var apiErr *msg.APIError
	ck.True(errors.As(err, &apiErr), "an idle timeout is an APIError, got %T: %v", err, err)
	ck.Eq(msg.ErrTransient, apiErr.Kind, "kind")
	ck.StrContains(apiErr.Message, "idle timeout", "message names the watchdog")
}

// TestStreamLineCap pins that an over-long line is refused as non-retryable: the
// same request produces the same oversized line, so retrying only burns budget.
func TestStreamLineCap(t *testing.T) {
	ck := assert.NewAborting(t)
	body := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"" + string(bytes.Repeat([]byte("x"), 4096)) + "\"}}\n\n")
	d := responses.Codec{Mode: responses.ModeAPI}.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)), time.Second, 64)
	defer func() { ck.NoError(d.Close(), "close decoder") }()

	_, err := d.Next()
	var apiErr *msg.APIError
	ck.True(errors.As(err, &apiErr), "an over-long line is an APIError, got %T: %v", err, err)
	ck.Eq(msg.ErrOther, apiErr.Kind, "kind is not retryable")
	ck.StrContains(apiErr.Message, "exceeds 64 bytes", "message names the cap")
}

// TestStreamTruncated pins that a stream which stops before a terminal event is a
// transient failure rather than a short but successful turn.
func TestStreamTruncated(t *testing.T) {
	ck := assert.NewAborting(t)
	body := sse(`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`)
	d := responses.Codec{Mode: responses.ModeAPI}.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)), time.Second, 0)
	defer func() { ck.NoError(d.Close(), "close decoder") }()

	acc := msg.NewAccumulator()
	for {
		ev, err := d.Next()
		if err == nil {
			ck.NoError(acc.Add(ev), "accumulate")
			continue
		}
		var apiErr *msg.APIError
		ck.True(errors.As(err, &apiErr), "a truncated stream is an APIError, got %T: %v", err, err)
		ck.Eq(msg.ErrTransient, apiErr.Kind, "kind")
		ck.StrContains(apiErr.Message, "truncated", "message names the truncation")
		return
	}
}

// TestClassify pins the classification a sender acts on, for HTTP responses and
// in-band payloads alike.
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   msg.ErrorKind
	}{
		{name: "usage limit code on a 429 is not a rate limit", status: http.StatusTooManyRequests, body: `{"error":{"code":"usage_limit_reached"}}`, want: msg.ErrUsageLimit},
		{name: "usage limit type", status: http.StatusBadRequest, body: `{"error":{"type":"usage_not_included"}}`, want: msg.ErrUsageLimit},
		{name: "insufficient quota", status: http.StatusTooManyRequests, body: `{"code":"insufficient_quota"}`, want: msg.ErrUsageLimit},
		{name: "context length", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded","message":"too long"}}`, want: msg.ErrPromptTooLarge},
		{name: "context length in a failed response", body: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`, want: msg.ErrPromptTooLarge},
		{name: "rate limit code in a body", status: http.StatusBadRequest, body: `{"error":{"code":"rate_limit_exceeded"}}`, want: msg.ErrRateLimit},
		{name: "rate limit type in an in-band event", body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, want: msg.ErrRateLimit},
		{name: "rate limit code in a failed response", body: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`, want: msg.ErrRateLimit},
		{name: "plain rate limit", status: http.StatusTooManyRequests, body: `{"error":{"message":"slow down"}}`, want: msg.ErrRateLimit},
		{name: "server error", status: http.StatusInternalServerError, body: ``, want: msg.ErrTransient},
		{name: "bad gateway", status: http.StatusBadGateway, body: `not json at all`, want: msg.ErrTransient},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_request_error"}}`, want: msg.ErrOther},
		{name: "unclassified success", status: http.StatusOK, body: `{}`, want: msg.ErrOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ck.Eq(tc.want, responses.Classify(tc.status, tc.header, []byte(tc.body)), "kind")
		})
	}
}

// TestUsageLimitFromResponse pins the HTTP-side constructor: only a 429 or a 404
// can carry a usage limit, the plan may arrive as a header, and the reset time
// follows whichever rate-limit window the endpoint says is binding.
func TestUsageLimitFromResponse(t *testing.T) {
	ck := assert.NewAborting(t)
	body := []byte(`{"error":{"code":"usage_limit_reached","message":"out of budget"}}`)
	header := http.Header{
		"X-Codex-Plan-Type":                   []string{"plus"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
	}

	limit := responses.UsageLimitFromResponse(http.StatusTooManyRequests, header, body)
	ck.NotZero(limit, "a 429 with a usage-limit body carries the limit")
	ck.Eq("usage_limit_reached", limit.Code, "code")
	ck.Eq("plus", limit.PlanType, "the plan comes from the header")
	ck.True(limit.ResetsAt.After(time.Now()), "the reset time comes from the header")

	ck.Zero(responses.UsageLimitFromResponse(http.StatusBadRequest, header, body), "only a 429 or a 404 carries a usage limit")
	ck.Zero(responses.UsageLimitFromResponse(http.StatusTooManyRequests, header, []byte(`{"error":{"code":"rate_limit_exceeded"}}`)),
		"a plain 429 is not a usage limit")

	// The endpoint names the window that is in force, and that window's headers
	// are the ones carrying the reset.
	other := http.Header{
		"X-Codex-Active-Limit":                      []string{"codex_other"},
		"X-Codex-Primary-Reset-After-Seconds":       []string{"3600"},
		"X-Codex-Other-Primary-Reset-After-Seconds": []string{"60"},
	}
	otherLimit := responses.UsageLimitFromResponse(http.StatusNotFound, other, body)
	ck.NotZero(otherLimit, "a 404 carries it too")
	ck.True(otherLimit.ResetsAt.Before(time.Now().Add(5*time.Minute)), "the binding window's reset wins")
}

// accumulate decodes one SSE body through the codec and folds it into a response.
func accumulate(t *testing.T, mode responses.Mode, body []byte) *msg.Response {
	t.Helper()
	ck := assert.NewAborting(t)
	d := responses.Codec{Mode: mode}.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)), time.Minute, 0)
	defer func() { ck.NoError(d.Close(), "close decoder") }()
	acc := msg.NewAccumulator()
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			return acc.Response()
		}
		ck.NoError(err, "next event")
		ck.NoError(acc.Add(ev), "accumulate %T", ev)
	}
}

// drainErr decodes body until the decoder reports its terminal error.
func drainErr(t *testing.T, mode responses.Mode, body []byte) error {
	t.Helper()
	ck := assert.NewAborting(t)
	d := responses.Codec{Mode: mode}.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)), time.Minute, 0)
	defer func() { ck.NoError(d.Close(), "close decoder") }()
	for {
		_, err := d.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("the stream ended cleanly; expected a terminal error")
		}
		if err != nil {
			return err
		}
	}
}

// sse renders payloads as one text/event-stream body, each frame terminated by a
// blank line.
func sse(payloads ...string) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		buf.WriteString("data: ")
		buf.WriteString(p)
		buf.WriteString("\n\n")
	}
	return buf.Bytes()
}
