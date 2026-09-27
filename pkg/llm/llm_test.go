// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

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

func rateLimitErrWithRetryAfter(secs int) *anthropic.Error {
	e := &anthropic.Error{StatusCode: http.StatusTooManyRequests}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	e.Request = req
	e.Response = &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {strconv.Itoa(secs)}},
	}
	_ = e.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Provider returned error"}}`))
	return e
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
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
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
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
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
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
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
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", openrouter),
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

// TestSendParamsPinnedModelCarriesProviderPrefs proves a provider-pinned
// slash model (routing provider pins) reaches the OpenRouter sender with the
// "provider" extra field set, and an unpinned one does not.
func TestSendParamsPinnedModelCarriesProviderPrefs(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
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
	ck.StrContains(wire(0), `"provider":{"only":["fireworks"]}`, "pinned model wire body missing provider pin")
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
		WithProviderSender("anthropic", primary),
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
		WithProviderSender("anthropic", anthropicSender),
		WithProviderSender("openrouter", openrouter),
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
	ck.Eq("claude-sonnet-5", string(anthropicSender.lastReq[0].Model), "resolved wire model")
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
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
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
	// SeedHistory idempotence: re-seeding identical prefix no-ops.
	params := make([]Message, 0, 4)
	for _, m := range history {
		params = append(params, m.Param)
	}
	ck.NoError(conv.SeedHistory(context.Background(), params), "idempotent re-seed failed")
	// Resume plumbing must refuse without a store.
	if _, err := conv.IncrementResumeAttempts(context.Background()); err == nil {
		t.Error("IncrementResumeAttempts must error without WithStore")
	}
}

func TestIsPromptTooLarge(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True(isPromptTooLarge(promptTooLargeErr()), "fabricated prompt-too-long error not recognized")
	c.False(isPromptTooLarge(authErr()), "401 misclassified as prompt-too-large")
	c.False(isPromptTooLarge(nil), "nil misclassified")
}

func TestDefaultTrimPolicyKeepsFirstAndRecent(t *testing.T) {
	c := assert.NewCollecting(t)
	big := strings.Repeat("x", 60*1024)
	msgs := make([]Message, 0, 8)
	for range 8 {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(big)))
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
	small := []Message{
		anthropic.NewUserMessage(anthropic.NewTextBlock("a")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("b")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("c")),
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
		system: []anthropic.TextBlockParam{
			{Text: "part one"},
			{Text: "part two"},
		},
	}}
	temp := 0.7
	var k int64 = 40
	conv.cfg.temperature, conv.cfg.topK = &temp, &k

	msgs := UserTextMessages("hi")
	params := conv.assemble(msgs, sendConfig{maxTokens: 1024})
	c.Eq(1024, params.MaxTokens, "per-send max_tokens override lost")
	// Default policy: one 5m breakpoint on the LAST system block.
	var withCC int
	for i, b := range params.System {
		if b.CacheControl.Type != "" || b.CacheControl.TTL != "" {
			withCC++
			c.Eq(len(params.System)-1, i, "breakpoint on block")
			c.Eq("", b.CacheControl.TTL, "TTL")
		}
	}
	c.Eq(1, withCC, "%d system cache breakpoints, want exactly 1", withCC)
	// Default policy: moving breakpoint on the request's last message block —
	// on the assembled request only; the caller's messages stay untouched.
	last := params.Messages[len(params.Messages)-1].Content
	if cc := last[len(last)-1].GetCacheControl(); cc == nil || cc.Type == "" {
		t.Error("no moving breakpoint on the last message block")
	}
	origLast := msgs[len(msgs)-1].Content
	if cc := origLast[len(origLast)-1].GetCacheControl(); cc != nil && cc.Type != "" {
		t.Error("assemble mutated the caller's message blocks")
	}
	// The conversation's configured system must NOT be mutated by assembly.
	c.Eq("", conv.cfg.system[len(conv.cfg.system)-1].CacheControl.Type, "assemble mutated the conversation's system blocks")
	v, _ := params.Temperature.Value, false
	c.Eq(0.7, v, "temperature")
}

func TestAssembleCachePolicyVariants(t *testing.T) {
	c := assert.NewCollecting(t)
	system := []anthropic.TextBlockParam{{Text: "sys"}}

	// 1h system, messages off.
	conv := &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: Cache1h, MessagesTTL: CacheOff, Breakpoints: 1}}}
	params := conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	c.Eq(anthropic.CacheControlEphemeralTTLTTL1h, params.System[0].CacheControl.TTL, "system TTL")
	mLast := params.Messages[0].Content
	if cc := mLast[len(mLast)-1].GetCacheControl(); cc != nil && cc.Type != "" {
		t.Error("messages off: unexpected moving breakpoint")
	}

	// Everything off.
	conv = &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: CacheOff, MessagesTTL: CacheOff, Breakpoints: 1}}}
	params = conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	c.Eq("", params.System[0].CacheControl.Type, "system off: unexpected breakpoint")
}

func TestWithMessageBreakpoints(t *testing.T) {
	c := assert.NewCollecting(t)
	policy := &CachePolicy{MessagesTTL: Cache5m, Breakpoints: 2}

	markedAt := func(msgs []Message) []int {
		var marked []int
		for i := range msgs {
			if cc := msgs[i].Content[0].GetCacheControl(); cc != nil && cc.Type != "" {
				marked = append(marked, i)
			}
		}
		return marked
	}

	// A block-heavy history: 30 single-block user messages.
	var msgs []Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock("b")))
	}
	out := withMessageBreakpoints(msgs, policy)
	marked := markedAt(out)
	c.Require().Len(marked, 2, "marked")
	c.Eq(len(msgs)-1, marked[len(marked)-1], "last message not marked: %v", marked)
	c.GreaterOrEqual(lookbackStride, marked[1]-marked[0], "breakpoint gap")
	// Copy-on-write: the input history carries no markers.
	c.Nil(markedAt(msgs), "input mutated: markers at")

	// A history reloaded from capture carries stale markers verbatim; they
	// must be cleared on the assembled request (4-breakpoint API limit).
	stale := withMessageBreakpoints(out, policy) // out has markers baked in
	c.Len(markedAt(stale), 2, "stale markers not consolidated")

	// One more message MOVES the markers on the new request.
	grown := append(append([]Message{}, msgs...), anthropic.NewUserMessage(anthropic.NewTextBlock("new")))
	out2 := withMessageBreakpoints(grown, policy)
	m2 := markedAt(out2)
	if len(m2) != 2 || m2[len(m2)-1] != len(grown)-1 {
		t.Errorf("after growth: markers %v, want last=%d", m2, len(grown)-1)
	}

	// Off policy still scrubs stale markers.
	off := withMessageBreakpoints(out, &CachePolicy{MessagesTTL: CacheOff, Breakpoints: 1})
	c.Nil(markedAt(off), "off policy left markers")
}

// UserTextMessages is a test helper: one user message wrapping UserText.
func UserTextMessages(s string) []Message {
	return []Message{anthropic.NewUserMessage(anthropic.NewTextBlock(s))}
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
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
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
