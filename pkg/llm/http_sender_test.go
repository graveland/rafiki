// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// minimalRequest is the smallest native request the senders accept.
func minimalRequest() *msg.Request { return userTextRequest("gpt-4o") }

// nativeRequest is minimalRequest for callers that want a *testing.T helper.
func nativeRequest(t *testing.T) *msg.Request {
	t.Helper()
	return minimalRequest()
}

// mustJSON marshals v for assertions and error messages; on failure it says
// so instead of panicking inside a t.Errorf argument.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable: " + err.Error() + ">"
	}
	return string(b)
}

// mustJSONString JSON-encodes s as a wire string value.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// sseHandler serves a pre-built text/event-stream body.
func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// anthropicOK is a minimal well-formed Anthropic response body.
const anthropicOK = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

// TestHTTPSenderMakesOneAttemptOn529 pins that the sender never retries on its
// own: retry policy belongs to the layers above, which know whether anything
// was already delivered.
func TestHTTPSenderMakesOneAttemptOn529(t *testing.T) {
	c := assert.NewAborting(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()

	sender, err := SenderForKey(providers.Provider{
		Name: "a", Kind: providers.KindAnthropic, BaseURL: srv.URL,
	}, "sk-test", nil)
	c.Require().NoError(err, "SenderForKey")

	_, err = sender.Send(context.Background(), nativeRequest(t), msg.EncodeOptions{})
	c.Require().Error(err, "Send must fail on 529")

	var apiErr *msg.APIError
	c.Require().True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
	c.Eq(529, apiErr.Status)
	c.Eq(msg.ErrOverloaded, apiErr.Kind)
	c.Eq(int32(1), hits.Load(), "attempts")
}

// TestHTTPSenderMapsRateLimitWithRetryAfter pins that a 429 refusal reaches the
// layers above with everything the rate-limit loop and the operator need: the
// status, the rate-limit class, the upstream's Retry-After as a duration, the
// dialect that refused, and the provider's own message. Dropping the
// Retry-After loses the backoff the rate-limit loop waits on, and dropping the
// message leaves the operator with a bare status.
func TestHTTPSenderMapsRateLimitWithRetryAfter(t *testing.T) {
	const providerMessage = "Number of requests exceeded your rate limit"
	cases := []struct {
		name    string
		kind    providers.Kind
		dialect msg.Dialect
		body    string
	}{
		{
			name:    "anthropic",
			kind:    providers.KindAnthropic,
			dialect: msg.DialectAnthropic,
			body:    `{"type":"error","error":{"type":"rate_limit_error","message":"` + providerMessage + `"}}`,
		},
		{
			name:    "openai chat",
			kind:    providers.KindOpenAI,
			dialect: msg.DialectOpenAIChat,
			body:    `{"error":{"code":429,"type":"rate_limit_exceeded","message":"` + providerMessage + `"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			sender, err := SenderForKey(providers.Provider{
				Name: "p", Kind: tc.kind, BaseURL: srv.URL,
			}, "sk-test", nil)
			c.Require().NoError(err, "SenderForKey")

			_, err = sender.Send(context.Background(), nativeRequest(t), msg.EncodeOptions{})
			c.Require().Error(err, "Send must fail on 429")

			var apiErr *msg.APIError
			c.Require().True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
			c.Eq(http.StatusTooManyRequests, apiErr.Status, "status")
			c.Eq(msg.ErrRateLimit, apiErr.Kind, "kind")
			c.Eq(7*time.Second, apiErr.RetryAfter, "Retry-After header as a duration")
			c.Eq(tc.dialect, apiErr.Dialect, "dialect")
			c.Eq(providerMessage, apiErr.Message, "the provider's own error message")
		})
	}
}

// TestHTTPSenderSetsSessionHeaderOpenRouter pins the sticky-routing header that
// keeps a whole conversation on one backend for prompt-cache locality. Every
// OpenRouter sender must carry it, and SenderForKey is the only correct way to
// get one.
func TestHTTPSenderSetsSessionHeaderOpenRouter(t *testing.T) {
	c := assert.NewAborting(t)
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOK)
	}))
	defer srv.Close()

	sender, err := SenderForKey(providers.Provider{
		Name: "openrouter", Kind: providers.KindAnthropicOpenRouter, BaseURL: srv.URL,
	}, "sk-or-1", nil)
	c.Require().NoError(err, "SenderForKey")

	ctx := WithSessionID(context.Background(), "conv-abc-123")
	_, err = sender.Send(ctx, nativeRequest(t), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")
	c.Eq("conv-abc-123", gotSession, "x-session-id")
}

// TestHTTPSenderKeylessSendsNoAuth pins the keyless convention on the openai
// kind: an empty key sends NO credential header at all, never "Bearer " with an
// empty value, which is what lets an unauthenticated local server work.
func TestHTTPSenderKeylessSendsNoAuth(t *testing.T) {
	c := assert.NewAborting(t)
	var sawAuth, sawAPIKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		_, sawAPIKey = r.Header["X-Api-Key"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sender, err := SenderForKey(providers.Provider{
		Name: "oa", Kind: providers.KindOpenAI, BaseURL: srv.URL,
	}, "", nil)
	c.Require().NoError(err, "SenderForKey")

	_, err = sender.Send(context.Background(), nativeRequest(t), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")
	c.False(sawAuth, "Authorization header present with no key")
	c.False(sawAPIKey, "x-api-key header present with no key")
}

// TestHTTPSenderCapturesHeaders pins that headerCaptureTransport still fills
// the raw-trace capture point for a native sender — the raw trace is the only
// record of what actually went over the wire.
func TestHTTPSenderCapturesHeaders(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "together")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOK)
	}))
	defer srv.Close()

	sender, err := SenderForKey(providers.Provider{
		Name: "a", Kind: providers.KindAnthropic, BaseURL: srv.URL,
	}, "sk-secret", nil)
	c.Require().NoError(err, "SenderForKey")

	ctx, hdrs := withRawTraceHeaders(context.Background())
	_, err = sender.Send(ctx, nativeRequest(t), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")

	c.Require().NotNil(hdrs.req, "request headers were not captured")
	c.Eq("sk-secret", hdrs.req.Get("X-Api-Key"), "captured credential header")
	c.Eq("2023-06-01", hdrs.req.Get("Anthropic-Version"), "captured anthropic-version")
	c.Require().NotNil(hdrs.resp, "response headers were not captured")
	c.Eq("together", hdrs.resp.Get("X-Upstream"), "captured response header")
}

// TestHTTPSenderAnthropicStaticHeaders pins the plain Anthropic wire: the
// version header, a JSON content type, and the SDK's own X-Api-Key spelling.
func TestHTTPSenderAnthropicStaticHeaders(t *testing.T) {
	c := assert.NewAborting(t)
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOK)
	}))
	defer srv.Close()

	sender, err := SenderForKey(providers.Provider{
		Name: "a", Kind: providers.KindAnthropic, BaseURL: srv.URL,
	}, "sk-test", nil)
	c.Require().NoError(err, "SenderForKey")
	_, err = sender.Send(context.Background(), nativeRequest(t), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")
	c.Eq("2023-06-01", got.Get("Anthropic-Version"))
	c.Eq("application/json", got.Get("Content-Type"))
	c.Eq("sk-test", got.Get("X-Api-Key"))
}

// TestAnthropicBearerSenderSendsBearerNoAPIKey pins the one credential shape
// rafiki's own /v1/messages proxy accepts: a bearer token and no x-api-key,
// whatever ANTHROPIC_API_KEY the environment holds.
func TestAnthropicBearerSenderSendsBearerNoAPIKey(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-environment-leak")
	var got http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOK)
	}))
	defer srv.Close()

	sender := NewAnthropicBearerSender(srv.URL, "rafiki-token", nil)
	_, err := sender.Send(context.Background(), nativeRequest(t), msg.EncodeOptions{})
	c.Require().NoError(err, "Send")

	c.Eq("Bearer rafiki-token", got.Get("Authorization"), "Authorization")
	c.Eq("", got.Get("X-Api-Key"), "x-api-key must never be sent to the proxy")
	c.Eq("2023-06-01", got.Get("Anthropic-Version"), "anthropic-version")
	c.Eq("/v1/messages", gotPath, "proxy path")
}

// TestHTTPSenderOpenAIRequiresBaseURL pins the constructor contract: a generic
// OpenAI-compatible endpoint has no canonical default.
func TestHTTPSenderOpenAIRequiresBaseURL(t *testing.T) {
	c := assert.NewAborting(t)
	_, err := SenderForKey(providers.Provider{Name: "oa", Kind: providers.KindOpenAI}, "k", nil)
	c.Require().Error(err, "SenderForKey(openai) with no base_url")
	c.StrContains(err.Error(), "base_url", "error names the missing setting")
}

// openAITestSet is a one-provider registry pointing at srv.
func openAITestSet(t *testing.T, srv *httptest.Server) *providers.Set {
	t.Helper()
	toml := fmt.Sprintf("default_provider = \"oa\"\n\n[providers.oa]\nkind = \"openai\"\nbase_url = %q\n", srv.URL)
	set, err := providers.Parse([]byte(toml))
	assert.NewAborting(t).NoError(err, "parse the openai test registry")
	return set
}

// openAIConversation builds an in-memory Conversation whose one provider is a
// real openai-kind sender pointed at srv.
func openAIConversation(t *testing.T, srv *httptest.Server, opts ...ClientOption) (*Conversation, *Client) {
	t.Helper()
	ck := assert.NewAborting(t)
	set := openAITestSet(t, srv)
	sender, err := SenderForKey(set.Providers["oa"], "", nil)
	ck.NoError(err, "SenderForKey(oa)")
	clientOpts := append([]ClientOption{
		WithProviders(set),
		WithProviderSender("oa", sender),
		WithDefaultModel("oa/gpt-4o"),
		WithLogger(testLogger(t)),
	}, opts...)
	client, err := NewClient(clientOpts...)
	ck.NoError(err, "NewClient")
	conv, err := client.Conversation(context.Background(),
		NewConversation("", "test"), Model("oa/gpt-4o"), SystemText("sys"))
	ck.NoError(err, "Conversation")
	return conv, client
}

// openAIChatOK is a minimal well-formed Chat Completions response body.
const openAIChatOK = `{"id":"c1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"after trim"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`

// TestTrimRetryOnOpenAIChatPromptTooLarge pins that a context overflow from a
// Chat Completions endpoint is classified ErrPromptTooLarge by the codec, so
// Conversation.Send trims and retries instead of failing the turn.
func TestTrimRetryOnOpenAIChatPromptTooLarge(t *testing.T) {
	c := assert.NewAborting(t)
	var hits atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if hits.Load() == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens"}}`)
			return
		}
		_, _ = io.WriteString(w, openAIChatOK)
	}))
	defer srv.Close()

	conv, _ := openAIConversation(t, srv)
	seedBigHistory(t, conv, 5)

	got, err := conv.Send(context.Background(), UserText("question"))
	c.Require().NoError(err, "Send must trim and retry, not fail")
	c.Eq("after trim", responseText(got), "message")
	c.Eq(int32(2), hits.Load(), "attempts")
	c.Require().Len(bodies, 2, "captured bodies")
	first := strings.Count(bodies[0], `"role"`)
	second := strings.Count(bodies[1], `"role"`)
	c.Less(first, second, "the retry must carry fewer messages than the rejected attempt")
}

// TestNoEligibleDecoratesOpenAIChat pins that the no-eligible class reaches the
// decorator for a Chat Completions refusal too: the error names the model and
// the routing spec that excluded every provider, and stays non-retryable.
func TestNoEligibleDecoratesOpenAIChat(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"No endpoints found for gpt-4o"}}`)
	}))
	defer srv.Close()

	conv, _ := openAIConversation(t, srv, WithRouting(routing.Spec{Sort: routing.SortPrice}))
	_, err := conv.Send(context.Background(), UserText("hi"))
	c.Require().Error(err, "the no-endpoints rejection must surface")
	c.StrContains(err.Error(), "no provider can serve gpt-4o under routing [sort=price]: ",
		"the error must name the model and the spec, got: %s", err)

	var apiErr *msg.APIError
	c.True(errors.As(err, &apiErr), "the API error must stay reachable through the wrap: %v", err)
	c.Eq(msg.ErrNoEligible, apiErr.Kind)
	c.False(routing.Retryable(err), "deterministic: the same request fails forever")
	c.False(routing.FailoverWorthy(err), "a 404 is not an upstream-health signal")
}

// openAIInBandRateLimit is a stream that rejects the request after the
// connection is up — the shape an upstream 429 takes mid-stream.
const openAIInBandRateLimit = "data: {\"id\":\"c1\",\"model\":\"m\",\"error\":{\"code\":429,\"type\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n"

// TestStreamInBandRateLimitUsesModelGate pins that an in-band rate limit on the
// HTTP path reaches the ModelGate, not a generic per-turn retry: the gate blocks
// every send for that model while it backs off.
func TestStreamInBandRateLimitUsesModelGate(t *testing.T) {
	c := assert.NewAborting(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		sseHandler(openAIInBandRateLimit)(w, r)
	}))
	defer srv.Close()

	conv, client := openAIConversation(t, srv)
	client.modelGate = NewModelGate(10*time.Millisecond, 20*time.Millisecond)
	conv.cfg.rateLimit = RateLimitPolicy{MaxRetries: 1}

	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(*msg.Response) {}))
	c.Require().Error(err, "Send must fail once the retries are exhausted")

	isRL, _ := isRateLimit(err)
	c.True(isRL, "the in-band 429 must surface as a rate limit, got: %v", err)
	c.Eq(int32(2), hits.Load(), "attempts (one retry through the gate)")
	c.True(modelGateBlocked(client.modelGate, "oa/gpt-4o"), "the gate must have recorded the 429 for the model")
}

// TestStreamInBandTransientIsRetryable pins the HTTP-level shape of a transient
// in-band rejection: it becomes a status-0 APIError the retry classifier reads.
func TestStreamInBandTransientIsRetryable(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(sseHandler(
		"data: {\"id\":\"c1\",\"model\":\"m\",\"error\":{\"code\":500,\"type\":\"server_error\",\"message\":\"upstream exploded\"}}\n\n"))
	defer srv.Close()

	conv, _ := openAIConversation(t, srv)
	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(*msg.Response) {}))
	c.Require().Error(err, "Send must fail")

	var apiErr *msg.APIError
	c.Require().True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
	c.Eq(0, apiErr.Status, "an in-band error carries no HTTP status")
	c.Eq(msg.ErrTransient, apiErr.Kind)
	c.True(IsTransientStreamError(err), "IsTransientStreamError")
	c.False(IsRateLimitStreamError(err), "IsRateLimitStreamError")
}

// modelGateBlocked reports whether the gate currently holds a backoff for
// model. A recorded 429 is what makes a rate limit backpressure rather than a
// per-turn retry.
func modelGateBlocked(g *ModelGate, model string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.blockedUntil[model]
	return ok && time.Until(until) > -time.Minute
}

// roundTripFunc adapts a function to http.RoundTripper, so a test can hand a
// sender a response it could not get from a real server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// unreadableBody yields its bytes once, then fails: a refusal whose connection
// dies mid-body.
type unreadableBody struct {
	data []byte
	done bool
}

func (b *unreadableBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, errors.New("read tcp: connection reset by peer")
	}
	b.done = true
	return copy(p, b.data), nil
}

func (b *unreadableBody) Close() error { return nil }

// TestHTTPSenderStreamRefusalWithUnreadableBody pins that a refusal whose body
// read FAILS is still reported as an *msg.APIError carrying its status, and that
// the bytes that did arrive are not read as evidence: this body names a 503,
// which must not turn the 400 into a retryable classification.
func TestHTTPSenderStreamRefusalWithUnreadableBody(t *testing.T) {
	c := assert.NewAborting(t)
	const body = `{"error":{"code":503}}`
	sender, err := SenderForKey(providers.Provider{
		Name: "oa", Kind: providers.KindOpenAI, BaseURL: "http://example.invalid",
	}, "sk-test", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       &unreadableBody{data: []byte(body)},
		}, nil
	}))
	c.Require().NoError(err, "SenderForKey")

	_, err = sender.Stream(context.Background(), nativeRequest(t), msg.EncodeOptions{})
	c.Require().Error(err, "Stream must report the refusal")

	var apiErr *msg.APIError
	c.Require().True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
	c.Eq(http.StatusBadRequest, apiErr.Status, "status")
	c.Eq(msg.ErrOther, apiErr.Kind, "an unreadable body must not classify as retryable from its bytes")
	c.StrContains(string(apiErr.Body), "503", "the bytes that arrived are kept for diagnosis")
}
