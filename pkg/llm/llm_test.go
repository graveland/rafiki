// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// scriptedSender returns queued results in order; the last repeats.
type scriptedSender struct {
	calls    int
	scripts  []func(*msg.Request) (*msg.Response, error)
	lastReq  []*msg.Request
	lastOpts []msg.EncodeOptions
	lastCtx  []context.Context
}

func (s *scriptedSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	s.lastReq = append(s.lastReq, req)
	s.lastOpts = append(s.lastOpts, opts)
	s.lastCtx = append(s.lastCtx, ctx)
	i := s.calls
	if i >= len(s.scripts) {
		i = len(s.scripts) - 1
	}
	s.calls++
	return s.scripts[i](req)
}

func (s *scriptedSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return llmtest.NewDecoder(llmtest.Events(resp)), nil
}

func respondText(text string) func(*msg.Request) (*msg.Response, error) {
	return func(*msg.Request) (*msg.Response, error) {
		return cannedMessage(`{"id":"msg_t","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":[{"type":"text","text":"` + text + `"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`), nil
	}
}

func respondErr(err error) func(*msg.Request) (*msg.Response, error) {
	return func(*msg.Request) (*msg.Response, error) { return nil, err }
}

// cannedMessage decodes an Anthropic response body into a native response.
func cannedMessage(raw string) *msg.Response {
	return llmtest.FromAnthropicJSON(raw)
}

// firstText returns the text of r's first text block.
func firstText(t *testing.T, r *msg.Response) string {
	t.Helper()
	for _, b := range r.Content {
		if tx, ok := b.(msg.Text); ok {
			return tx.Text
		}
	}
	t.Fatalf("response carries no text block: %+v", r.Content)
	return ""
}

// userTextRequest is the minimal native request the routing tests send.
func userTextRequest(model string) *msg.Request {
	return &msg.Request{Model: model, MaxTokens: 16,
		Messages: []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}}}
}

// overloadedErr fabricates a retryable overloaded refusal (529).
func overloadedErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusTooManyRequests + 100,
		Body:    []byte(`{"error":{"type":"overloaded_error","message":"Overloaded"}}`),
		Kind:    msg.ErrOverloaded,
		Dialect: msg.DialectAnthropic,
		Message: "Overloaded",
	}
}

// authErr fabricates a non-retryable refusal (401).
func authErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusUnauthorized,
		Body:    []byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
		Kind:    msg.ErrOther,
		Dialect: msg.DialectAnthropic,
		Message: "invalid x-api-key",
	}
}

// creditExhaustedErr fabricates the 400 Anthropic returns when the account has
// run out of credit: not retryable (the same request fails identically) but
// failover-worthy.
func creditExhaustedErr() *msg.APIError {
	const detail = "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."
	return &msg.APIError{
		Status:  http.StatusBadRequest,
		Body:    []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"` + detail + `"}}`),
		Kind:    msg.ErrOther,
		Dialect: msg.DialectAnthropic,
		Message: detail,
	}
}

func promptTooLargeErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusBadRequest,
		Body:    []byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long: too many tokens"}}`),
		Kind:    msg.ErrPromptTooLarge,
		Dialect: msg.DialectAnthropic,
		Message: "prompt is too long: too many tokens",
	}
}

func rateLimitErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusTooManyRequests,
		Body:    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Provider returned error"}}`),
		Kind:    msg.ErrRateLimit,
		Dialect: msg.DialectAnthropic,
		Message: "Provider returned error",
	}
}

// promptTooLargeAPIErr fabricates the native prompt-too-large refusal the
// classifiers read.
func promptTooLargeAPIErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusBadRequest,
		Body:    []byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long: too many tokens"}}`),
		Kind:    msg.ErrPromptTooLarge,
		Dialect: msg.DialectAnthropic,
		Message: "prompt is too long: too many tokens",
	}
}

// rateLimitAPIErrWithRetryAfter fabricates the native 429 the sender returns.
func rateLimitAPIErrWithRetryAfter(secs int) *msg.APIError {
	return &msg.APIError{
		Status:     http.StatusTooManyRequests,
		Kind:       msg.ErrRateLimit,
		RetryAfter: time.Duration(secs) * time.Second,
		Dialect:    msg.DialectAnthropic,
	}
}

func seededCatalog(t *testing.T) *routing.ModelCatalog {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
	})
	return cat
}

func TestNewClientDefaultsToBuiltInProviders(t *testing.T) {
	c, err := NewClient()
	assert.NewAborting(t).NoError(err, "NewClient with no options must use Default()")
	_ = c // healthy
}

func TestSendFailsOverAndMapsModel(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(overloadedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)

	req := userTextRequest("claude-haiku-4-5")
	resp, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil)
	ck.Require().NoError(err, "send")
	ck.Eq("from fallback", firstText(t, resp), "response")
	ck.Eq("anthropic/claude-haiku-4.5", fallback.lastReq[0].Model, "fallback model")
	ck.True(c.Breaker("anthropic").Open(), "breaker must be open after a retryable primary failure")

	// Breaker open → next call goes straight to the fallback (no probe yet).
	if _, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil); err != nil {
		t.Fatalf("send (pinned): %v", err)
	}
	ck.Eq(1, primary.calls, "primary called")
}

func TestSendNonRetryableDoesNotFailOver(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(authErr()),
	}}
	fallback := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	req := userTextRequest("claude-haiku-4-5")
	if _, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil); err == nil {
		t.Fatal("401 must surface, not fail over")
	}
	ck.Eq(0, fallback.calls, "fallback called")
	ck.False(c.Breaker("anthropic").Open(), "401 must not trip the breaker")
}

// An out-of-credit primary is not retryable, but it IS a reason to fail over:
// the account cannot answer any request until it is funded, so pinning callers
// to it would strand every send behind a billing problem.
func TestSendFailsOverWhenPrimaryOutOfCredit(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(creditExhaustedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	req := userTextRequest("claude-haiku-4-5")
	resp, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil)
	ck.Require().NoError(err, "send")
	ck.Eq("from fallback", firstText(t, resp), "response")
	ck.True(c.Breaker("anthropic").Open(), "breaker must be open after an out-of-credit primary rejection")
}

// TestSendSlashModelRoutesToOpenRouter proves an OpenRouter-native
// (slash) model — e.g. a resolved model alias like kimi-k3 — goes straight
// to the OpenRouter sender untranslated, never touching the Anthropic primary
// and never failing over (the caller asked for this specific model).
func TestSendSlashModelRoutesToOpenRouter(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("must not be reached"),
	}}
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", openrouter),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	req := userTextRequest("openrouter/moonshotai/kimi-k3")
	resp, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil)
	ck.Require().NoError(err, "send")
	ck.Eq("from openrouter", firstText(t, resp), "response")
	ck.Eq(0, primary.calls, "anthropic primary called")
	ck.Eq("moonshotai/kimi-k3", openrouter.lastReq[0].Model, "openrouter model")
}

// TestSendUnpinnedModelCarriesNoProviderPrefs proves an undeclared slash
// model reaches the OpenRouter sender with no "provider" extra field — the
// daemon ships no built-in per-line pin any more, so even a line that used to
// carry one sends none.
func TestSendUnpinnedModelCarriesNoProviderPrefs(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	send := func(model string) {
		t.Helper()
		req := userTextRequest(model)
		if _, err := c.send(context.Background(), SendMeta{}, req, nil); err != nil {
			t.Fatalf("send(%s): %v", model, err)
		}
	}
	send("openrouter/z-ai/glm-5.2")
	send("openrouter/moonshotai/kimi-k3")

	wire := func(i int) string {
		t.Helper()
		b, err := msganthropic.Encode(openrouter.lastReq[i], openrouter.lastOpts[i])
		ck.Require().NoError(err, "encode wire body")
		return string(b)
	}
	ck.NotStrContains(wire(0), `"provider"`, "no built-in pin: the body must carry no provider field")
	ck.NotStrContains(wire(1), `"provider"`, "unpinned model must not carry a provider field")
}

// A slash model with no OpenRouter sender configured must fail cleanly, not
// leak the request to the Anthropic API (which would 404 the model anyway).
func TestSendSlashModelWithoutOpenRouterErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	req := userTextRequest("deepseek/deepseek-v4-pro")
	if _, err := c.send(context.Background(), SendMeta{}, req, nil); err == nil {
		t.Fatal("slash model without an OpenRouter sender must error")
	}
	ck.Eq(0, primary.calls, "anthropic primary called")
}

// TestSendAnthropicPrefixRoutesNative proves the "anthropic/<x>" native
// marker reaches the direct Anthropic sender (prefix stripped on the wire),
// while a non-anthropic provider slash id still routes to OpenRouter — covering
// the second entry point where callers build params directly (bypassing
// ResolveModel).
func TestSendAnthropicPrefixRoutesNative(t *testing.T) {
	ck := assert.NewCollecting(t)
	anthropicSender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from anthropic"),
	}}
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", anthropicSender),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)

	// "anthropic/" prefix -> native Anthropic sender, prefix stripped on the wire.
	req := userTextRequest("anthropic/sonnet-latest")
	resp, err := c.send(context.Background(), SendMeta{}, req, nil)
	ck.Require().NoError(err, "send(anthropic/...)")
	ck.Eq("from anthropic", firstText(t, resp), "response")
	ck.Eq(0, openrouter.calls, "openrouter called")
	ck.Eq("sonnet-latest", anthropicSender.lastReq[0].Model, "anthropic wire model")

	// A non-anthropic provider slash id still routes to OpenRouter, unchanged.
	req2 := userTextRequest("openrouter/deepseek/deepseek-chat")
	resp2, err := c.send(context.Background(), SendMeta{}, req2, nil)
	ck.Require().NoError(err, "send(deepseek/...)")
	ck.Eq("from openrouter", firstText(t, resp2), "response")
	ck.Eq("deepseek/deepseek-chat", openrouter.lastReq[0].Model, "openrouter wire model")
	ck.Eq(1, anthropicSender.calls, "anthropic called")
}

// TestConversationAnthropicPrefixResolvesNative is the end-to-end check: an
// "anthropic/<family>-latest" model set on a conversation resolves (via
// ResolveModel at creation) to the concrete catalog id with the prefix gone,
// and the turn reaches the native Anthropic sender — never OpenRouter.
func TestConversationAnthropicPrefixResolvesNative(t *testing.T) {
	ck := assert.NewCollecting(t)
	anthropicSender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("native"),
	}}
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("openrouter"),
	}}
	c := newMemClient(t,
		WithProviderSender("anthropic", anthropicSender),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
	)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("anthropic/sonnet-latest"))
	ck.Require().NoError(err)
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	ck.Eq(1, anthropicSender.calls, "anthropic called")
	ck.Eq(0, openrouter.calls, "openrouter called")
	ck.Eq("claude-sonnet-5", anthropicSender.lastReq[0].Model, "resolved wire model")
}

// TestConversationNoModelNoDefaultErrors proves the hardcoded haiku default is
// gone: with no per-conversation model and no WithDefaultModel, creation errors
// loudly rather than silently selecting a model.
func TestConversationNoModelNoDefaultErrors(t *testing.T) {
	c, err := NewClient(
		WithProviderSender("anthropic", &scriptedSender{}),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	assert.NewAborting(t).NoError(err)
	if _, err := c.Conversation(context.Background(), NewConversation("", "test")); err == nil {
		t.Fatal("no per-conversation model + no default must error, not silently pick haiku")
	}
}

func TestSendNoFallbackBypassesBreaker(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(overloadedErr()), // trips via the WITH-fallback send
		respondText("direct despite pin"),
	}}
	fallback := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	req := userTextRequest("claude-haiku-4-5")

	// Trip the breaker via a fallback-configured send.
	if _, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, req, nil); err != nil {
		t.Fatalf("tripping send: %v", err)
	}
	ck.Require().True(c.Breaker("anthropic").Open(), "breaker should be open")

	// A send with NO fallback opts out of pinning: direct primary despite the
	// open breaker (the per-conversation escape hatch from the design).
	resp, err := c.send(context.Background(), SendMeta{Fallback: []string{}}, req, nil)
	ck.Require().NoError(err, "no-fallback send")
	ck.Eq("direct despite pin", firstText(t, resp), "response")
	ck.Eq(2, primary.calls, "primary calls")
}

func TestInMemoryConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("mem reply"),
		respondText("mem reply 2"),
	}}
	// No WithStore: conversation degrades to in-memory history.
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(context.Background(), NewConversation("", "cli"),
		Model("claude-haiku-4-5"), SystemText("sys"))
	ck.Require().NoError(err, "store-less Conversation")
	if _, err := conv.Send(context.Background(), UserText("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("two")); err != nil {
		t.Fatal(err)
	}
	// History accumulated in memory: second request carried 3 messages.
	ck.Eq(3, len(sender.lastReq[1].Messages), "second request messages")
	history, err := conv.History(context.Background())
	if err != nil || len(history) != 4 {
		t.Errorf("history = %d err=%v, want 4", len(history), err)
	}
	// SeedHistory idempotence: re-seeding identical prefix no-ops, and a
	// re-continue puts the head back on the reply it was on.
	params := []msg.Message{
		{Role: msg.User, Content: []msg.Block{msg.Text{Text: "one"}}},
		{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "mem reply"}}},
		{Role: msg.User, Content: []msg.Block{msg.Text{Text: "two"}}},
	}
	ck.NoError(conv.SeedHistory(context.Background(), params), "idempotent re-seed failed")
	if _, err := conv.Continue(context.Background()); err != nil {
		t.Fatalf("Continue after the re-seed: %v", err)
	}
	resumed, err := conv.History(context.Background())
	ck.Require().NoError(err)
	ck.EqDeep(history, resumed, "a re-seed and a re-continue must not change the working path")
	// Resume plumbing must refuse without a store.
	if _, err := conv.IncrementResumeAttempts(context.Background()); err == nil {
		t.Error("IncrementResumeAttempts must error without WithStore")
	}
}

func TestIsPromptTooLarge(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True(isPromptTooLarge(promptTooLargeAPIErr()), "fabricated prompt-too-long error not recognized")
	c.False(isPromptTooLarge(&msg.APIError{Status: http.StatusUnauthorized, Kind: msg.ErrOther}), "401 misclassified as prompt-too-large")
	c.False(isPromptTooLarge(nil), "nil misclassified")
}

func TestDefaultTrimPolicyKeepsFirstAndRecent(t *testing.T) {
	c := assert.NewCollecting(t)
	big := strings.Repeat("x", 60*1024)
	msgs := make([]msg.Message, 0, 8)
	for range 8 {
		msgs = append(msgs, msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: big}}})
	}
	p := defaultTrimPolicy{}

	trimmed, ok := p.Trim(msgs, 0) // 300KB budget: first + ~4 recent fit
	c.Require().True(ok, "trim must succeed on an oversized history")
	c.Require().Less(len(msgs), len(trimmed), "nothing trimmed")
	c.Eq(messageSize(msgs[0]), messageSize(trimmed[0]), "first message must be kept")
	// The kept tail must be the MOST RECENT messages, in order.
	c.Eq(messageSize(msgs[len(msgs)-1]), messageSize(trimmed[len(trimmed)-1]), "most recent message must be kept")

	// Escalating attempts shrink further.
	t2, ok := p.Trim(msgs, 2) // 75KB budget
	if !ok || len(t2) >= len(trimmed) {
		t.Errorf("attempt 2 should trim harder: %d vs %d (ok=%v)", len(t2), len(trimmed), ok)
	}

	// Two messages: nothing to drop.
	if _, ok := p.Trim(msgs[:2], 0); ok {
		t.Error("<=2 messages must report ok=false")
	}

	// Already within budget: dropping nothing must report ok=false, not spin.
	small := []msg.Message{
		{Role: msg.User, Content: []msg.Block{msg.Text{Text: "a"}}},
		{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "b"}}},
		{Role: msg.User, Content: []msg.Block{msg.Text{Text: "c"}}},
	}
	if _, ok := p.Trim(small, 0); ok {
		t.Error("no-op trim must report ok=false")
	}
}

func TestAssembleAppliesCachePolicy(t *testing.T) {
	c := assert.NewCollecting(t)
	conv := &Conversation{cfg: convConfig{
		model:     "claude-haiku-4-5",
		maxTokens: 16384,
		system: []msg.Text{
			{Text: "part one"},
			{Text: "part two"},
		},
	}}
	temp := 0.7
	var k int64 = 40
	conv.cfg.temperature, conv.cfg.topK = &temp, &k

	msgs := UserTextMessages("hi")
	req := conv.assemble(msgs, sendConfig{maxTokens: 1024})
	c.Eq(int64(1024), req.MaxTokens, "per-send max_tokens override lost")
	c.Eq(DefaultCachePolicy(), req.Cache, "the request carries the conversation's cache policy")
	c.Require().Len(req.System, 2, "system blocks")
	c.Eq("part two", req.System[1].Text, "system text")
	c.Eq(0.7, *req.Temperature, "temperature")
	c.Eq(int64(40), *req.TopK, "top_k")
	// The codec places the markers the policy asks for: the default policy's
	// one moving breakpoint sits on the request's last message block.
	positions := msganthropic.Breakpoints(req)
	c.Require().Len(positions, 1, "moving breakpoints")
	c.Eq(0, positions[0].Message, "moving breakpoint message")
	c.Eq(0, positions[0].Block, "moving breakpoint block")

	// And one 5m system breakpoint, on the LAST system block.
	body, err := msganthropic.Encode(req, msg.EncodeOptions{})
	c.Require().NoError(err, "encode request")
	var wire struct {
		System []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	c.Require().NoError(json.Unmarshal(body, &wire), "decode body")
	c.Require().Len(wire.System, 2)
	c.Eq(0, len(wire.System[0].CacheControl), "breakpoint must be on the LAST system block")
	c.NotEmpty(wire.System[1].CacheControl, "no system cache breakpoint")
	// The conversation's own system blocks are values: assembly hands out a
	// copy, so nothing a codec does can reach the config.
	c.Eq("part two", conv.cfg.system[1].Text, "assemble mutated the conversation's system blocks")
}

func TestAssembleCachePolicyVariants(t *testing.T) {
	c := assert.NewCollecting(t)
	system := []msg.Text{{Text: "sys"}}

	// 1h system, messages off.
	conv := &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: Cache1h, MessagesTTL: CacheOff, Breakpoints: 1}}}
	req := conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	c.Eq(Cache1h, req.Cache.SystemTTL, "system TTL")
	c.Nil(msganthropic.Breakpoints(req), "messages off: unexpected moving breakpoint")

	// Everything off.
	conv = &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: CacheOff, MessagesTTL: CacheOff, Breakpoints: 1}}}
	req = conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	c.Eq(CacheOff, req.Cache.SystemTTL, "system off")
	c.Nil(msganthropic.Breakpoints(req), "everything off: unexpected moving breakpoint")
}

// UserTextMessages is a test helper: one native user message wrapping UserText.
func UserTextMessages(s string) []msg.Message {
	return []msg.Message{{Role: msg.User, Content: UserText(s)}}
}

// ---- Task 2 primitives: Primary + ThinkingBudget conv options -------------

// newMemClient builds a store-less (in-memory) client for unit tests: full
// loop semantics, no DB. Callers add their own WithUpstream options.
func newMemClient(t *testing.T, opts ...ClientOption) *Client {
	t.Helper()
	base := []ClientOption{WithLogger(testLogger(t)), WithDefaultModel("claude-test")}
	c, err := NewClient(append(base, opts...)...)
	assert.NewAborting(t).NoError(err)
	return c
}

func TestPrimaryOptionRoutesUpstream(t *testing.T) {
	ck := assert.NewAborting(t)
	anthropicSender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from anthropic"),
	}}
	openrouterSender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("from openrouter"),
	}}
	c := newMemClient(t,
		WithProviderSender("anthropic", anthropicSender),
		WithProviderSender("openrouter", openrouterSender),
	)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-test"), Primary("openrouter"))
	ck.NoError(err)
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	ck.Eq(1, openrouterSender.calls, "openrouter (declared primary) called")
	ck.Eq(0, anthropicSender.calls, "anthropic called")
}

func TestThinkingBudgetSetsParam(t *testing.T) {
	ck := assert.NewAborting(t)
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"),
	}}
	c := newMemClient(t, WithProviderSender("anthropic", sender))
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-test"), ThinkingBudget(8192))
	ck.NoError(err)
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	ck.NotEmpty(sender.lastReq, "sender captured no request")
	last := sender.lastReq[len(sender.lastReq)-1]
	ck.Eq(int64(8192), last.ThinkingBudget, "thinking budget not set")
}

// TestProviderSurvivesDecode proves the non-standard "provider" field reaches
// the native response: the Anthropic SDK has no Provider field, so if the codec
// dropped unknown members the agent path could never attribute a cache miss to
// a provider.
func TestProviderSurvivesDecode(t *testing.T) {
	c := assert.NewCollecting(t)
	body := `{"id":"m1","type":"message","role":"assistant","model":"deepseek/deepseek-v4-pro",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1},"provider":"CoreWeave"}`
	resp, err := msganthropic.DecodeResponse([]byte(body))
	c.Require().NoError(err, "DecodeResponse")
	c.Eq("CoreWeave", ProviderOf(resp), "ProviderOf")
}

// TestProviderOfMissing proves a native Anthropic response yields no provider
// rather than a bogus one.
func TestProviderOfMissing(t *testing.T) {
	c := assert.NewCollecting(t)
	body := `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-4-8",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`
	resp, err := msganthropic.DecodeResponse([]byte(body))
	c.Require().NoError(err, "DecodeResponse")
	c.Eq("", ProviderOf(resp), "ProviderOf")
}
