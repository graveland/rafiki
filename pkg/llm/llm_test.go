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

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// sdkFake wraps an SDK-typed test sender so it satisfies the native Sender.
// The fakes stay SDK-shaped on purpose: they are the ones the client's own
// conversion is exercised against.
func sdkFake(s SDKSender) Sender { return FromSDKSender(s) }

// scriptedSender returns queued results in order; the last repeats.
type scriptedSender struct {
	calls   int
	scripts []func(params anthropic.MessageNewParams) (*anthropic.Message, error)
	lastReq []anthropic.MessageNewParams
	lastCtx []context.Context
}

func (s *scriptedSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.lastReq = append(s.lastReq, params)
	s.lastCtx = append(s.lastCtx, ctx)
	i := s.calls
	if i >= len(s.scripts) {
		i = len(s.scripts) - 1
	}
	s.calls++
	return s.scripts[i](params)
}

func respondText(text string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_t","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":[{"type":"text","text":"` + text + `"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`), nil
	}
}

func respondErr(err error) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return nil, err }
}

func cannedMessage(raw string) *anthropic.Message {
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return &m
}

// overloadedErr fabricates a retryable SDK error (529).
func overloadedErr() *anthropic.Error {
	e := &anthropic.Error{StatusCode: 529}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: 529}
	_ = e.UnmarshalJSON([]byte(`{"error":{"type":"overloaded_error","message":"Overloaded"}}`))
	return e
}

// authErr fabricates a non-retryable SDK error (401).
func authErr() *anthropic.Error {
	e := &anthropic.Error{StatusCode: http.StatusUnauthorized}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: http.StatusUnauthorized}
	_ = e.UnmarshalJSON([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	return e
}

// creditExhaustedErr fabricates the 400 Anthropic returns when the account has
// run out of credit: not retryable (the same request fails identically) but
// failover-worthy.
func creditExhaustedErr() *anthropic.Error {
	e := &anthropic.Error{StatusCode: http.StatusBadRequest}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: http.StatusBadRequest}
	_ = e.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`))
	return e
}

func promptTooLargeErr() *anthropic.Error {
	e := &anthropic.Error{StatusCode: http.StatusBadRequest}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: http.StatusBadRequest}
	_ = e.UnmarshalJSON([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long: too many tokens"}}`))
	return e
}

func rateLimitErr() *anthropic.Error {
	e := &anthropic.Error{StatusCode: http.StatusTooManyRequests}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: http.StatusTooManyRequests}
	_ = e.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Provider returned error"}}`))
	return e
}

// promptTooLargeAPIErr fabricates the native prompt-too-large refusal the
// classifiers read: the SDK-typed helper above goes through the compat adapter
// to produce exactly this.
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

func TestSendParamsFailsOverAndMapsModel(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(overloadedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithProviderSender("openrouter", sdkFake(fallback)),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)

	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params)
	ck.Require().NoError(err, "SendParams")
	ck.Eq("from fallback", resp.Content[0].Text, "response")
	ck.Eq("anthropic/claude-haiku-4.5", string(fallback.lastReq[0].Model), "fallback model")
	ck.True(c.Breaker("anthropic").Open(), "breaker must be open after a retryable primary failure")

	// Breaker open → next call goes straight to the fallback (no probe yet).
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params); err != nil {
		t.Fatalf("SendParams (pinned): %v", err)
	}
	ck.Eq(1, primary.calls, "primary called")
}

func TestSendParamsNonRetryableDoesNotFailOver(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(authErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithProviderSender("openrouter", sdkFake(fallback)),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params); err == nil {
		t.Fatal("401 must surface, not fail over")
	}
	ck.Eq(0, fallback.calls, "fallback called")
	ck.False(c.Breaker("anthropic").Open(), "401 must not trip the breaker")
}

// An out-of-credit primary is not retryable, but it IS a reason to fail over:
// the account cannot answer any request until it is funded, so pinning callers
// to it would strand every send behind a billing problem.
func TestSendParamsFailsOverWhenPrimaryOutOfCredit(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(creditExhaustedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithProviderSender("openrouter", sdkFake(fallback)),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params)
	ck.Require().NoError(err, "SendParams")
	ck.Eq("from fallback", resp.Content[0].Text, "response")
	ck.True(c.Breaker("anthropic").Open(), "breaker must be open after an out-of-credit primary rejection")
}

// TestSendParamsSlashModelRoutesToOpenRouter proves an OpenRouter-native
// (slash) model — e.g. a resolved model alias like kimi-k3 — goes straight
// to the OpenRouter sender untranslated, never touching the Anthropic primary
// and never failing over (the caller asked for this specific model).
func TestSendParamsSlashModelRoutesToOpenRouter(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithProviderSender("openrouter", sdkFake(openrouter)),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	params := anthropic.MessageNewParams{Model: "openrouter/moonshotai/kimi-k3", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params)
	ck.Require().NoError(err, "SendParams")
	ck.Eq("from openrouter", resp.Content[0].Text, "response")
	ck.Eq(0, primary.calls, "anthropic primary called")
	ck.Eq("moonshotai/kimi-k3", string(openrouter.lastReq[0].Model), "openrouter model")
}

// TestSendParamsUnpinnedModelCarriesNoProviderPrefs proves an undeclared slash
// model reaches the OpenRouter sender with no "provider" extra field — the
// daemon ships no built-in per-line pin any more, so even a line that used to
// carry one sends none.
func TestSendParamsUnpinnedModelCarriesNoProviderPrefs(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(&scriptedSender{})),
		WithProviderSender("openrouter", sdkFake(openrouter)),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	send := func(model string) {
		t.Helper()
		params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 16,
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
		if _, err := c.SendParams(context.Background(), SendMeta{}, params); err != nil {
			t.Fatalf("SendParams(%s): %v", model, err)
		}
	}
	send("openrouter/z-ai/glm-5.2")
	send("openrouter/moonshotai/kimi-k3")

	wire := func(i int) string {
		t.Helper()
		b, err := json.Marshal(openrouter.lastReq[i])
		ck.Require().NoError(err, "marshal wire params")
		return string(b)
	}
	ck.NotStrContains(wire(0), `"provider"`, "no built-in pin: the body must carry no provider field")
	ck.NotStrContains(wire(1), `"provider"`, "unpinned model must not carry a provider field")
}

// A slash model with no OpenRouter sender configured must fail cleanly, not
// leak the request to the Anthropic API (which would 404 the model anyway).
func TestSendParamsSlashModelWithoutOpenRouterErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	params := anthropic.MessageNewParams{Model: "deepseek/deepseek-v4-pro", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	if _, err := c.SendParams(context.Background(), SendMeta{}, params); err == nil {
		t.Fatal("slash model without an OpenRouter sender must error")
	}
	ck.Eq(0, primary.calls, "anthropic primary called")
}

// TestSendParamsAnthropicPrefixRoutesNative proves the "anthropic/<x>" native
// marker reaches the direct Anthropic sender (prefix stripped on the wire),
// while a non-anthropic provider slash id still routes to OpenRouter — covering
// the second entry point where callers build params directly (bypassing
// ResolveModel).
func TestSendParamsAnthropicPrefixRoutesNative(t *testing.T) {
	ck := assert.NewCollecting(t)
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from anthropic"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(anthropicSender)),
		WithProviderSender("openrouter", sdkFake(openrouter)),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)

	// "anthropic/" prefix -> native Anthropic sender, prefix stripped on the wire.
	params := anthropic.MessageNewParams{Model: "anthropic/sonnet-latest", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{}, params)
	ck.Require().NoError(err, "SendParams(anthropic/...)")
	ck.Eq("from anthropic", resp.Content[0].Text, "response")
	ck.Eq(0, openrouter.calls, "openrouter called")
	ck.Eq("sonnet-latest", string(anthropicSender.lastReq[0].Model), "anthropic wire model")

	// A non-anthropic provider slash id still routes to OpenRouter, unchanged.
	params2 := anthropic.MessageNewParams{Model: "openrouter/deepseek/deepseek-chat", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp2, err := c.SendParams(context.Background(), SendMeta{}, params2)
	ck.Require().NoError(err, "SendParams(deepseek/...)")
	ck.Eq("from openrouter", resp2.Content[0].Text, "response")
	ck.Eq("deepseek/deepseek-chat", string(openrouter.lastReq[0].Model), "openrouter wire model")
	ck.Eq(1, anthropicSender.calls, "anthropic called")
}

// TestConversationAnthropicPrefixResolvesNative is the end-to-end check: an
// "anthropic/<family>-latest" model set on a conversation resolves (via
// ResolveModel at creation) to the concrete catalog id with the prefix gone,
// and the turn reaches the native Anthropic sender — never OpenRouter.
func TestConversationAnthropicPrefixResolvesNative(t *testing.T) {
	ck := assert.NewCollecting(t)
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("native"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("openrouter"),
	}}
	c := newMemClient(t,
		WithProviderSender("anthropic", sdkFake(anthropicSender)),
		WithProviderSender("openrouter", sdkFake(openrouter)),
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
	ck.Eq("claude-sonnet-5", string(anthropicSender.lastReq[0].Model), "resolved wire model")
}

// TestConversationNoModelNoDefaultErrors proves the hardcoded haiku default is
// gone: with no per-conversation model and no WithDefaultModel, creation errors
// loudly rather than silently selecting a model.
func TestConversationNoModelNoDefaultErrors(t *testing.T) {
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(&scriptedSender{})),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	assert.NewAborting(t).NoError(err)
	if _, err := c.Conversation(context.Background(), NewConversation("", "test")); err == nil {
		t.Fatal("no per-conversation model + no default must error, not silently pick haiku")
	}
}

func TestSendParamsNoFallbackBypassesBreaker(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(overloadedErr()), // trips via the WITH-fallback send
		respondText("direct despite pin"),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(primary)),
		WithProviderSender("openrouter", sdkFake(fallback)),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}

	// Trip the breaker via a fallback-configured send.
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, params); err != nil {
		t.Fatalf("tripping send: %v", err)
	}
	ck.Require().True(c.Breaker("anthropic").Open(), "breaker should be open")

	// A send with NO fallback opts out of pinning: direct primary despite the
	// open breaker (the per-conversation escape hatch from the design).
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []string{}}, params)
	ck.Require().NoError(err, "no-fallback send")
	ck.Eq("direct despite pin", resp.Content[0].Text, "response")
	ck.Eq(2, primary.calls, "primary calls")
}

func TestInMemoryConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("mem reply"),
		respondText("mem reply 2"),
	}}
	// No WithStore: conversation degrades to in-memory history.
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(sender)),
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
	params := []Message{
		anthropic.NewUserMessage(anthropic.NewTextBlock("one")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("mem reply")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("two")),
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
	return []msg.Message{{Role: msg.User, Content: UserTextMsg(s)}}
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
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from anthropic"),
	}}
	openrouterSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c := newMemClient(t,
		WithProviderSender("anthropic", sdkFake(anthropicSender)),
		WithProviderSender("openrouter", sdkFake(openrouterSender)),
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
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := newMemClient(t, WithProviderSender("anthropic", sdkFake(sender)))
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-test"), ThinkingBudget(8192))
	ck.NoError(err)
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	ck.NotEmpty(sender.lastReq, "sender captured no request")
	last := sender.lastReq[len(sender.lastReq)-1]
	ck.False(last.Thinking.OfEnabled == nil || last.Thinking.OfEnabled.BudgetTokens != 8192, "thinking budget not set: %+v", last.Thinking)
}

// TestProviderSurvivesSDKDecode proves the non-standard "provider" field
// reaches Go through the SDK's ExtraFields. This is the one genuine unknown in
// the design: the Anthropic SDK has no Provider field, so if it dropped unknown
// members the agent path could never attribute a cache miss to a provider.
func TestProviderSurvivesSDKDecode(t *testing.T) {
	c := assert.NewCollecting(t)
	var msg anthropic.Message
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"deepseek/deepseek-v4-pro",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1},"provider":"CoreWeave"}`)
	c.Require().NoError(json.Unmarshal(body, &msg), "unmarshal")
	c.Eq("CoreWeave", ProviderOf(&msg), "ProviderOf")
}

// TestProviderOfMissing proves a native Anthropic response yields no provider
// rather than a bogus one.
func TestProviderOfMissing(t *testing.T) {
	c := assert.NewCollecting(t)
	var msg anthropic.Message
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"claude-opus-4-8",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
	c.Require().NoError(json.Unmarshal(body, &msg), "unmarshal")
	c.Eq("", ProviderOf(&msg), "ProviderOf")
}
