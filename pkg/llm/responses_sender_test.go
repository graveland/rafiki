// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

const responsesFixtureTurn1 = "testdata/responses/std_turn1.response.sse"

// newResponsesSenderForTest builds a plain (api-key) sender against a fixture
// server, the way SenderForKey threads it together.
func newResponsesSenderForTest(t *testing.T, key string, handler http.HandlerFunc) *responsesSender {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := providers.Provider{Name: "fixture", Kind: providers.KindOpenAIResponses, BaseURL: srv.URL}
	s, err := newResponsesSender(p, key, nil)
	assert.NewAborting(t).NoError(err, "newResponsesSender")
	return s
}

// newChatGPTResponsesSenderForTest builds the owner-bound sender and points it at
// a fixture server (the real base URL is fixed; only tests move it).
func newChatGPTResponsesSenderForTest(t *testing.T, ts TokenSource, facts ModelFacts, handler http.HandlerFunc) *responsesSender {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	sender, err := NewChatGPTResponsesSender(providers.Provider{Name: "chatgpt", Kind: providers.KindOpenAIResponsesChatGPT}, ts, facts, nil)
	assert.NewAborting(t).NoError(err, "NewChatGPTResponsesSender")
	s, ok := sender.(*responsesSender)
	assert.NewAborting(t).True(ok, "NewChatGPTResponsesSender returned %T, want *responsesSender", sender)
	s.baseURL = srv.URL
	return s
}

// recordingTokenSource counts Token calls and the force flag of each, and can
// hand back headers alongside the bearer (the ChatGPT account id).
type recordingTokenSource struct {
	mu      sync.Mutex
	forces  []bool
	bearer  string
	headers http.Header
}

func (r *recordingTokenSource) Token(_ context.Context, force bool) (string, http.Header, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forces = append(r.forces, force)
	return r.bearer, r.headers, nil
}

func (r *recordingTokenSource) calls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.forces...)
}

// responsesSSE joins JSON payloads into an SSE body. The `event:` line is
// omitted on purpose: the decoder reads the type from the payload, which is the
// shape most real events arrive in.
func responsesSSE(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

// responsesCompleted is a minimal well-formed terminal event.
func responsesCompleted(extra string) string {
	return `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"` + extra + `}}`
}

// readRequestBody reads a request body, transparently decompressing the zstd
// encoding ChatGPT mode sends. It returns bytes rather than a decoded map, and
// asserts nothing: request handlers run on another goroutine, where t.Fatalf is
// not allowed.
func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	assert.NewAborting(t).NoError(err, "read request body")
	if r.Header.Get("Content-Encoding") == "zstd" {
		dec, derr := zstd.NewReader(nil)
		assert.NewAborting(t).NoError(derr, "zstd reader")
		defer dec.Close()
		raw, err = dec.DecodeAll(raw, nil)
		assert.NewAborting(t).NoError(err, "zstd decode")
	}
	return raw
}

// decodeMap unmarshals a recorded body into a map.
func decodeMap(c *assert.C, raw []byte, what string) map[string]any {
	var body map[string]any
	c.Require().NoError(json.Unmarshal(raw, &body), "unmarshal %s", what)
	return body
}

// responsesRequest converts the SDK params these tests speak into the native
// request the sender takes.
func responsesRequest(t *testing.T, params anthropic.MessageNewParams) *msg.Request {
	t.Helper()
	req, _, err := msganthropic.RequestFromSDK(params)
	assert.NewAborting(t).NoError(err, "RequestFromSDK")
	return req
}

// responsesSend issues one non-streamed call through s.
func responsesSend(t *testing.T, s *responsesSender, params anthropic.MessageNewParams) (*msg.Response, error) {
	t.Helper()
	return s.Send(context.Background(), responsesRequest(t, params), msg.EncodeOptions{})
}

// responsesStream opens one streamed call through s and drains it.
func responsesStream(t *testing.T, s *responsesSender, params anthropic.MessageNewParams) (*msg.Response, error) {
	t.Helper()
	dec, err := s.Stream(context.Background(), responsesRequest(t, params), msg.EncodeOptions{})
	if err != nil {
		return nil, err
	}
	return drainResponses(t, dec)
}

// drainResponses accumulates a native stream into one response.
func drainResponses(t *testing.T, dec msg.StreamDecoder) (*msg.Response, error) {
	t.Helper()
	ck := assert.NewAborting(t)
	defer func() {
		if cerr := dec.Close(); cerr != nil {
			ck.NoError(cerr, "close stream")
		}
	}()
	acc := msg.NewAccumulator()
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		ck.NoError(acc.Add(ev), "accumulate %T", ev)
	}
	return acc.Response(), nil
}

// TestResponsesStreamTranslatesFixture streams a recorded api.openai.com
// response through the sender: the accumulated native response carries the
// reasoning signature, the tool call keyed by call_id, the stop reason and the
// usage the fixture reported.
func TestResponsesStreamTranslatesFixture(t *testing.T) {
	c := assert.NewAborting(t)
	body, err := os.ReadFile(responsesFixtureTurn1)
	c.NoError(err, "read fixture")

	s := newResponsesSenderForTest(t, "k", sseHandler(string(body)))
	resp, err := responsesStream(t, s, minimalParams())
	c.Require().NoError(err, "stream")

	c.Eq("resp_0cf3ceb209f8671c016ae18df08954819ba356b3c5b74b1d53", resp.ID, "id")
	c.Eq("gpt-6.1-sol", resp.Model, "model")
	c.Eq(msg.StopToolUse, resp.StopReason, "stop reason")
	c.Require().Len(resp.Content, 2, "content blocks")

	thinking, ok := resp.Content[0].(msg.Thinking)
	c.Require().True(ok, "block 0 is %T, want msg.Thinking", resp.Content[0])
	mode, id, blob, sigOK := ParseResponsesSignature(thinking.Sig.Value)
	c.True(sigOK, "signature %q is not a Responses signature", thinking.Sig.Value)
	c.Eq(ResponsesModeAPI, mode, "signature mode")
	c.Eq("rs_0cf3ceb209f8671c016ae18df1d0b8819baece265f64916983", id, "item id")
	c.NotEmpty(blob, "encrypted content")

	toolUse, ok := resp.Content[1].(msg.ToolUse)
	c.Require().True(ok, "block 1 is %T, want msg.ToolUse", resp.Content[1])
	c.Eq("call_70b6c42145b04a3eac6712e9e23c36df", toolUse.ID, "call id")
	c.Eq("get_weather", toolUse.Name, "name")
	c.EqDiff(json.RawMessage(`{"city":"Oslo"}`), toolUse.Input, "input")

	c.Eq(int64(100), msg.Val(resp.Usage.Input), "input tokens")
	c.Eq(int64(35), msg.Val(resp.Usage.Output), "output tokens")
}

// TestResponsesNonStreamingResponse pins the non-streamed api.openai.com path:
// Send returns one JSON response object, translated into the same native shapes
// the streaming path produces.
func TestResponsesNonStreamingResponse(t *testing.T) {
	// The full response: reasoning with encrypted content, text, a tool call.
	const fullBody = `{"id":"resp_ns","model":"gpt-6.1-sol","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"weighing"}],"encrypted_content":"blob-1"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling get_weather"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}
	],"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":1},"output_tokens":7}}`

	// A reasoning item with NO encrypted_content: the non-streamed twin of the
	// always-sign rule. It must still become a thinking block, signed with an
	// empty blob, so an Anthropic failover strips it instead of rejecting it.
	const noEncryptedBody = `{"id":"resp_ns2","model":"gpt-6.1-sol","status":"completed","output":[
		{"type":"reasoning","id":"rs_nosig","summary":[]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
	],"usage":{"input_tokens":3,"output_tokens":2}}`

	cases := []struct {
		name  string
		body  string
		check func(c *assert.C, resp *msg.Response)
	}{
		{
			name: "reasoning, text and a tool call",
			body: fullBody,
			check: func(c *assert.C, resp *msg.Response) {
				c.Require().Len(resp.Content, 3, "blocks")
				thinking, ok := resp.Content[0].(msg.Thinking)
				c.Require().True(ok, "block 0 is %T", resp.Content[0])
				c.Eq("weighing", thinking.Text, "thinking text")
				mode, id, blob, sigOK := ParseResponsesSignature(thinking.Sig.Value)
				c.True(sigOK, "signature %q is not a Responses signature", thinking.Sig.Value)
				c.Eq(ResponsesModeAPI, mode, "signature mode")
				c.Eq("rs_1", id, "item id")
				c.Eq("blob-1", blob, "blob")

				text, ok := resp.Content[1].(msg.Text)
				c.Require().True(ok, "block 1 is %T", resp.Content[1])
				c.Eq("calling get_weather", text.Text, "text")

				toolUse, ok := resp.Content[2].(msg.ToolUse)
				c.Require().True(ok, "block 2 is %T", resp.Content[2])
				c.Eq("call_1", toolUse.ID, "tool_use id")
				c.Eq("get_weather", toolUse.Name, "tool name")

				c.Eq(msg.StopToolUse, resp.StopReason, "stop reason")
				c.Eq(int64(15), msg.Val(resp.Usage.Input), "input tokens (20 - 5 cached)")
				c.Eq(int64(5), msg.Val(resp.Usage.CacheRead), "cache read tokens")
				c.Eq(int64(7), msg.Val(resp.Usage.Output), "output tokens")
			},
		},
		{
			name: "reasoning with no encrypted content is still signed",
			body: noEncryptedBody,
			check: func(c *assert.C, resp *msg.Response) {
				c.Require().Len(resp.Content, 2, "blocks")
				thinking, ok := resp.Content[0].(msg.Thinking)
				c.Require().True(ok, "block 0 is %T", resp.Content[0])
				c.Eq(ResponsesSignaturePrefix+"api:rs_nosig:", thinking.Sig.Value,
					"a reasoning item with no encrypted_content must still be signed, with an empty blob")
				text, ok := resp.Content[1].(msg.Text)
				c.Require().True(ok, "block 1 is %T", resp.Content[1])
				c.Eq("done", text.Text, "text")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			var method, path string
			s := newResponsesSenderForTest(t, "sk", func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			resp, err := responsesSend(t, s, minimalParams())
			c.Require().NoError(err, "Send")
			c.Eq("POST", method, "method")
			c.Eq("/responses", path, "path")
			c.Eq("gpt-6.1-sol", resp.Model, "model")
			c.NotEmpty(resp.ID, "response id")
			tc.check(c, resp)
		})
	}
}

// TestResponsesChatGPTModeRequest pins the ChatGPT dialect's wire: store:false,
// stream:true, encrypted reasoning included, prompt_cache_key and the session-id
// header both naming the session, the rafiki originator, and the TokenSource's
// own headers.
func TestResponsesChatGPTModeRequest(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu     sync.Mutex
		raw    []byte
		hdr    http.Header
		path   string
		method string
	)
	ts := &recordingTokenSource{bearer: "tok-1", headers: http.Header{"Chatgpt-Account-Id": {"acct-1"}}}
	s := newChatGPTResponsesSenderForTest(t, ts, nil, func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		mu.Lock()
		raw = body
		hdr = r.Header.Clone()
		path, method = r.URL.Path, r.Method
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})

	ctx := WithSessionID(context.Background(), "sess-42")
	_, err := s.Send(ctx, responsesRequest(t, minimalParams()), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")

	mu.Lock()
	defer mu.Unlock()
	got := decodeMap(c, raw, "request body")
	c.Eq("/responses", path, "path")
	c.Eq("POST", method, "method")
	c.Eq("zstd", hdr.Get("Content-Encoding"), "Content-Encoding")
	c.Eq("sess-42", hdr.Get("session-id"), "session-id header")
	c.Eq("rafiki", hdr.Get("originator"), "originator")
	c.Eq("acct-1", hdr.Get("Chatgpt-Account-Id"), "TokenSource header")
	c.Eq("Bearer tok-1", hdr.Get("Authorization"), "Authorization")
	c.Eq("text/event-stream", hdr.Get("Accept"), "Accept")
	c.NotContains(hdr.Values("x-openai-internal-codex-responses-lite"), "true", "lite header must never be sent")
	c.NotHasKey(hdr, "OpenAI-Beta", "OpenAI-Beta is an older codex header and must not be sent")
	c.EqDiff(false, got["store"], "store")
	c.EqDiff(true, got["stream"], "stream")
	c.EqDiff(any([]any{"reasoning.encrypted_content"}), got["include"], "include")
	c.EqDiff("sess-42", got["prompt_cache_key"], "prompt_cache_key")
}

// TestResponsesRetriesOnce401 pins the 401 rule: force a credential refresh and
// retry exactly once. A second 401 is the answer.
func TestResponsesRetriesOnce401(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu       sync.Mutex
		requests int
	)
	ts := &recordingTokenSource{bearer: "tok"}
	s := newChatGPTResponsesSenderForTest(t, ts, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})
	_, err := s.Send(context.Background(), responsesRequest(t, minimalParams()), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")
	c.EqDiff([]bool{false, true}, ts.calls(), "Token force flags")
	c.Eq(2, requests, "requests")

	// Two 401s: the error is returned as-is after exactly two requests.
	mu.Lock()
	requests = 0
	mu.Unlock()
	ts2 := &recordingTokenSource{bearer: "tok"}
	s2 := newChatGPTResponsesSenderForTest(t, ts2, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err = s2.Send(context.Background(), responsesRequest(t, minimalParams()), msg.EncodeOptions{})
	c.Require().Error(err, "Send with two 401s")
	var apiErr *msg.APIError
	c.True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
	c.Eq(http.StatusUnauthorized, apiErr.Status, "status")
	c.Eq(2, requests, "requests with a repeated 401")
	c.EqDiff([]bool{false, true}, ts2.calls(), "Token force flags with a repeated 401")
}

// TestResponsesKeylessSendsNoAuthorization pins the keyless convention: an empty
// key sends NO Authorization header (never "Bearer ").
func TestResponsesKeylessSendsNoAuthorization(t *testing.T) {
	c := assert.NewAborting(t)
	var hdr http.Header
	s := newResponsesSenderForTest(t, "", func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"m","status":"completed","output":[]}`)
	})
	_, err := responsesSend(t, s, minimalParams())
	c.Require().NoError(err, "Send")
	c.NotHasKey(hdr, "Authorization", "Authorization header")
	c.Eq("", hdr.Get("Authorization"), "Authorization")
}

// TestResponsesUsageLimitIsTerminal pins a subscription usage limit as its own
// terminal type: never retried, invisible to both retry classifiers, and
// distinguishable from an ordinary 429.
func TestResponsesUsageLimitIsTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	const limitBody = `{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","plan_type":"plus","resets_at":1900000000,"limit_window_minutes":300}}`

	limitErr := func(t *testing.T, status int, body string, hdr http.Header) error {
		t.Helper()
		s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
			for k, vs := range hdr {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
		_, err := responsesSend(t, s, minimalParams())
		assert.NewAborting(t).Error(err, "Send should fail")
		return err
	}

	err := limitErr(t, http.StatusTooManyRequests, limitBody, nil)
	var u *UsageLimitError
	c.True(errors.As(err, &u), "error %T is not a *UsageLimitError", err)
	c.Eq("usage_limit_reached", u.Code, "code")
	c.Eq("plus", u.PlanType, "plan")
	c.Eq(time.Unix(1900000000, 0).UTC().Format(time.RFC3339), u.ResetsAt.UTC().Format(time.RFC3339), "resets_at")
	var apiErr *msg.APIError
	c.False(errors.As(err, &apiErr), "UsageLimitError must not be a *msg.APIError")
	c.StrContains(err.Error(), "provider usage limit reached (usage_limit_reached, plan plus); resets at ", "error text")
	for _, forbidden := range []string{"EOF", "connection reset", "broken pipe", "i/o timeout", "tls:"} {
		c.NotStrContains(err.Error(), forbidden, "error text must not contain %q (agentloop would retry it)", forbidden)
	}
	isRL, _ := isRateLimit(err)
	c.False(isRL, "isRateLimit")
	c.False(IsRateLimitStreamError(err), "IsRateLimitStreamError")

	// The same body on a 404 is the same terminal error.
	err = limitErr(t, http.StatusNotFound, limitBody, nil)
	c.True(errors.As(err, &u), "404: error %T is not a *UsageLimitError", err)
	c.Eq("usage_limit_reached", u.Code, "404 code")

	// A 429 without a usage-limit code stays a retryable rate limit.
	err = limitErr(t, http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, nil)
	c.True(errors.As(err, &apiErr), "plain 429: error %T is not a *msg.APIError", err)
	c.Eq(http.StatusTooManyRequests, apiErr.Status, "plain 429 status")
	c.Eq(msg.ErrRateLimit, apiErr.Kind, "plain 429 kind")
	isRL, _ = isRateLimit(err)
	c.True(isRL, "plain 429 must stay retryable")

	// A 503 stays a 503.
	err = limitErr(t, http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, nil)
	c.True(errors.As(err, &apiErr), "503: error %T is not a *msg.APIError", err)
	c.Eq(http.StatusServiceUnavailable, apiErr.Status, "503 status")

	// Header fallback: the family is chosen by x-codex-active-limit, and
	// codex_other reads the other-primary headers.
	before := time.Now()
	err = limitErr(t, http.StatusTooManyRequests, limitBody[:strings.Index(limitBody, `,"resets_at"`)]+`}}`, http.Header{
		"X-Codex-Active-Limit":                      {"codex_other"},
		"X-Codex-Other-Primary-Reset-After-Seconds": {"3600"},
		"X-Codex-Primary-Reset-After-Seconds":       {"60"},
	})
	c.True(errors.As(err, &u), "header fallback: error %T is not a *UsageLimitError", err)
	reset := u.ResetsAt.Sub(before)
	c.True(reset > 55*time.Minute && reset < 65*time.Minute,
		"resets_at from the codex_other family = %v after the request, want about 1h", reset)
}

// TestResponsesHTTPContextOverflowIsPromptTooLarge pins that a non-streamed
// context overflow reaches trim's predicate, so TrimPolicy fires on this
// provider instead of the turn failing.
func TestResponsesHTTPContextOverflowIsPromptTooLarge(t *testing.T) {
	c := assert.NewAborting(t)
	s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`)
	})
	_, err := responsesSend(t, s, minimalParams())
	c.Require().Error(err, "Send")
	c.True(isPromptTooLarge(err), "a context overflow must trigger the trim predicate, got: %v", err)
	c.True(IsPromptTooLarge(err), "IsPromptTooLarge")
}

// TestResponsesTerminalEvents pins the terminal-event table: each class of
// in-band failure becomes the error class the layers above this package act on.
func TestResponsesTerminalEvents(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_1","model":"m"}}`
	cases := []struct {
		name  string
		sse   string
		check func(c *assert.C, resp *msg.Response, err error)
	}{
		{
			name: "incomplete with max_output_tokens stops with max_tokens",
			sse: responsesSSE(created,
				`{"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens":5}}}`),
			check: func(c *assert.C, resp *msg.Response, err error) {
				c.Require().NoError(err, "stream error")
				c.Eq(msg.StopMaxTokens, resp.StopReason, "stop reason")
				c.Eq(int64(6), msg.Val(resp.Usage.Input), "input tokens (10 - 4 cached)")
				c.Eq(int64(4), msg.Val(resp.Usage.CacheRead), "cache read")
				c.Eq(int64(2), msg.Val(resp.Usage.CacheWrite), "cache write")
			},
		},
		{
			name: "content filter is terminal and not retried",
			sse: responsesSSE(created,
				`{"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`),
			check: func(c *assert.C, _ *msg.Response, err error) {
				c.Require().Error(err, "a content-filter stop must fail the turn")
				c.False(IsTransientStreamError(err), "content filter must not be retried")
				c.False(IsRateLimitStreamError(err), "content filter is not a rate limit")
			},
		},
		{
			name: "failed with usage_limit_reached is a UsageLimitError",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"id":"resp_1","model":"m","status":"failed","error":{"type":"usage_limit_reached","code":"usage_limit_reached"}}}`),
			check: func(c *assert.C, _ *msg.Response, err error) {
				var u *UsageLimitError
				c.True(errors.As(err, &u), "error %T is not a *UsageLimitError", err)
				c.False(IsTransientStreamError(err), "a usage limit is terminal")
			},
		},
		{
			name: "a rate limit is ErrRateLimit, never the generic transient class",
			sse: responsesSSE(created,
				`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`),
			check: func(c *assert.C, _ *msg.Response, err error) {
				c.Require().Error(err, "stream error")
				c.True(IsRateLimitStreamError(err), "IsRateLimitStreamError")
				c.False(IsTransientStreamError(err), "a rate limit is not the generic transient class")
				isRL, _ := isRateLimit(err)
				c.True(isRL, "isRateLimit")
			},
		},
		{
			// ErrOther is the safe default: retrying an error nobody understood is
			// how one failure becomes a storm.
			name: "an unknown code is terminal",
			sse: responsesSSE(created,
				`{"type":"error","code":"something_new","message":"who knows"}`),
			check: func(c *assert.C, _ *msg.Response, err error) {
				c.Require().Error(err, "stream error")
				var apiErr *msg.APIError
				c.True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
				c.Eq(msg.ErrOther, apiErr.Kind, "kind")
				c.False(IsTransientStreamError(err), "an unknown code is not retried")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			s := newResponsesSenderForTest(t, "k", sseHandler(tc.sse))
			resp, err := responsesStream(t, s, minimalParams())
			tc.check(c, resp, err)
		})
	}
}

// TestResponsesLargeEventLine pins the reader's line cap: output_item.done and
// completed carry whole output items, encrypted reasoning included, on ONE data
// line, and bufio.Scanner's 64 KiB default would fail them.
func TestResponsesLargeEventLine(t *testing.T) {
	c := assert.NewAborting(t)
	huge := strings.Repeat("x", 5<<20)
	sse := responsesSSE(
		`{"type":"response.created","response":{"id":"resp_big","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":`+mustJSONString(huge)+`}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+mustJSONString(huge)+`}]}}`,
		responsesCompleted(``),
	)
	s := newResponsesSenderForTest(t, "k", sseHandler(sse))
	resp, err := responsesStream(t, s, minimalParams())
	c.Require().NoError(err, "stream")
	c.Require().Len(resp.Content, 1, "content blocks")
	text, ok := resp.Content[0].(msg.Text)
	c.Require().True(ok, "block 0 is %T", resp.Content[0])
	c.Eq(len(huge), len(text.Text), "accumulated text length")
}

// TestResponsesLineCapOverflowNotRetried pins that an over-long line is
// deterministic: the same request produces it again, so it is never retried.
func TestResponsesLineCapOverflowNotRetried(t *testing.T) {
	c := assert.NewAborting(t)
	s := newResponsesSenderForTest(t, "k", sseHandler(
		"data: "+strings.Repeat("x", 4096)+"\n\n"))
	s.maxLineBytes = 1024

	_, err := responsesStream(t, s, minimalParams())
	c.Require().Error(err, "an over-long line must fail the stream")
	c.False(IsTransientStreamError(err), "an over-long line is deterministic and must not be retried")
	c.False(IsRateLimitStreamError(err), "an over-long line is not a rate limit")
}

// TestResponsesStreamIdleTimeout pins the idle timeout: a backend that goes
// silent mid-frame is a transient failure, not a hung turn, and the half frame it
// left behind is never dispatched.
func TestResponsesStreamIdleTimeout(t *testing.T) {
	c := assert.NewAborting(t)
	done := make(chan struct{})
	s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One complete frame, then the beginning of a second one: the data line
		// arrives, its blank line never does.
		_, _ = io.WriteString(w, responsesSSE(`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`))
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"leaked\"}\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-done:
		}
	})
	t.Cleanup(func() { close(done) })
	s.streamIdleTimeout = 50 * time.Millisecond

	dec, err := s.Stream(context.Background(), responsesRequest(t, minimalParams()), msg.EncodeOptions{})
	c.Require().NoError(err, "Stream")
	defer func() { _ = dec.Close() }()

	acc := msg.NewAccumulator()
	var gotErr error
	for {
		ev, derr := dec.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			gotErr = derr
			break
		}
		c.NoError(acc.Add(ev), "accumulate %T", ev)
	}
	c.Require().Error(gotErr, "the silent backend must surface as an error")
	c.True(IsTransientStreamError(gotErr), "IsTransientStreamError")

	resp := acc.Response()
	for _, b := range resp.Content {
		if text, ok := b.(msg.Text); ok {
			c.NotEq("leaked", text.Text, "the partial frame's text reached the message")
		}
	}
}

// TestResponsesTurnStateEcho pins x-codex-turn-state: remembered per session and
// replayed on that session's next request, never on another session's.
func TestResponsesTurnStateEcho(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu   sync.Mutex
		seen []string
		n    int
	)
	s := newChatGPTResponsesSenderForTest(t, &recordingTokenSource{bearer: "tok"}, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		seen = append(seen, r.Header.Get("x-codex-turn-state"))
		state := "ts-" + string(rune('0'+n))
		mu.Unlock()
		w.Header().Set("x-codex-turn-state", state)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})

	ctxA := WithSessionID(context.Background(), "sess-a")
	ctxB := WithSessionID(context.Background(), "sess-b")
	req := responsesRequest(t, minimalParams())
	_, err := s.Send(ctxA, req, msg.EncodeOptions{})
	c.Require().NoError(err, "first request")
	_, err = s.Send(ctxA, req, msg.EncodeOptions{})
	c.Require().NoError(err, "second request, same session")
	_, err = s.Send(ctxB, req, msg.EncodeOptions{})
	c.Require().NoError(err, "first request, other session")

	mu.Lock()
	defer mu.Unlock()
	c.Eq("", seen[0], "first request of a session carries no turn state")
	c.Eq("ts-1", seen[1], "second request of the same session replays the first response's turn state")
	c.Eq("", seen[2], "another session must not inherit a turn state")
}

// TestResponsesUserAgentAndCookies pins the two transport details a bot filter
// and the Cloudflare edge care about: a real User-Agent, and a per-sender cookie
// jar that replays a Set-Cookie within one sender and never across senders.
func TestResponsesUserAgentAndCookies(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu      sync.Mutex
		ua      []string
		cookies []string
		first   = true
	)
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ua = append(ua, r.Header.Get("User-Agent"))
		cookies = append(cookies, r.Header.Get("Cookie"))
		set := first
		first = false
		mu.Unlock()
		if set {
			http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "abc", Path: "/"})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	}

	// One handler, two senders: each has its own jar, so the cookie a sender
	// receives never reaches the other.
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	mk := func() *responsesSender {
		sender, err := NewChatGPTResponsesSender(providers.Provider{Name: "chatgpt"}, &recordingTokenSource{bearer: "t"}, nil, nil)
		assert.NewAborting(t).NoError(err, "NewChatGPTResponsesSender")
		s := sender.(*responsesSender)
		s.baseURL = srv.URL
		return s
	}
	a, b := mk(), mk()
	req := responsesRequest(t, minimalParams())

	_, err := a.Send(context.Background(), req, msg.EncodeOptions{})
	c.Require().NoError(err, "sender a, request 1")
	_, err = a.Send(context.Background(), req, msg.EncodeOptions{})
	c.Require().NoError(err, "sender a, request 2")
	_, err = b.Send(context.Background(), req, msg.EncodeOptions{})
	c.Require().NoError(err, "sender b, request 1")

	mu.Lock()
	defer mu.Unlock()
	c.Require().Len(ua, 3, "requests")
	for i, got := range ua {
		c.True(strings.HasPrefix(got, "rafiki/"), "request %d User-Agent = %q, want a rafiki/ prefix", i, got)
	}
	c.False(strings.Contains(cookies[0], "cf_clearance"), "first request carried a cookie: %q", cookies[0])
	c.StrContains(cookies[1], "cf_clearance=abc", "the jar must replay the cookie within one sender")
	c.False(strings.Contains(cookies[2], "cf_clearance"), "another sender's jar leaked a cookie: %q", cookies[2])
}

// TestResponsesSenderResendsOnlyOwnDialect pins the dialect gate at the HTTP
// level: a history carrying reasoning from BOTH Responses backends resends only
// the one this sender minted. The encrypted blob is meaningful only to the
// backend that produced it.
func TestResponsesSenderResendsOnlyOwnDialect(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu  sync.Mutex
		raw []byte
	)
	s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		mu.Lock()
		raw = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"m","status":"completed","output":[]}`)
	})

	params := minimalParams()
	params.Messages = []anthropic.MessageParam{
		{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
			{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "api reasoning", Signature: responsesSignature(ResponsesModeAPI, "rs_api", "blob-api")}},
			{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "chatgpt reasoning", Signature: responsesSignature(ResponsesModeChatGPT, "rs_gpt", "blob-gpt")}},
			{OfText: &anthropic.TextBlockParam{Text: "answer"}},
		}},
		{Role: "user", Content: []anthropic.ContentBlockParamUnion{
			{OfText: &anthropic.TextBlockParam{Text: "go on"}},
		}},
	}
	_, err := responsesSend(t, s, params)
	c.Require().NoError(err, "Send")

	mu.Lock()
	defer mu.Unlock()
	body := string(raw)
	c.StrContains(body, `"id":"rs_api"`, "this sender's own reasoning item must be resent")
	c.StrContains(body, "blob-api", "this sender's own encrypted content must be resent")
	c.NotStrContains(body, "rs_gpt", "the other dialect's reasoning item must be dropped")
	c.NotStrContains(body, "blob-gpt", "the other dialect's blob must be dropped")
}

// ---- the SDK direction ----

// TestSDKSenderDropsResponsesThinking pins the Anthropic direction: an sdkSender
// never sends a thinking block whose signature the Responses sender wrote.
func TestSDKSenderDropsResponsesThinking(t *testing.T) {
	c := assert.NewAborting(t)
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = raw
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	sender := sdkSender{client: anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("k"),
		option.WithMaxRetries(0),
	)}
	params := minimalParams()
	params.Messages = []anthropic.MessageParam{
		{Role: "user", Content: []anthropic.ContentBlockParamUnion{
			{OfText: &anthropic.TextBlockParam{Text: "hi"}},
		}},
		{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
			{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "foreign", Signature: responsesSignature(ResponsesModeAPI, "rs_1", "blob")}},
			{OfText: &anthropic.TextBlockParam{Text: "answer"}},
		}},
	}
	_, err := sender.New(context.Background(), params)
	c.Require().NoError(err, "sender.New")

	mu.Lock()
	defer mu.Unlock()
	c.NotStrContains(string(body), ResponsesSignaturePrefix, "the Anthropic request carried a Responses signature")
	c.StrContains(string(body), "answer", "the assistant text must survive the filter")
	// The caller's own history is untouched.
	c.Require().Len(params.Messages, 2, "caller messages")
	c.Require().Len(params.Messages[1].Content, 2, "caller message content (not mutated)")
}
