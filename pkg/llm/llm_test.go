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

	"go.graveland.dev/rafiki/pkg/routing"
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
}

func (s *scriptedSender) New(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.lastReq = append(s.lastReq, params)
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

func seededCatalog(t *testing.T) *routing.ModelCatalog {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
	})
	return cat
}

func TestNewClientRequiresAnthropic(t *testing.T) {
	if _, err := NewClient(); err == nil {
		t.Fatal("NewClient without an Anthropic sender must error")
	}
}

func TestSendParamsFailsOverAndMapsModel(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(overloadedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}

	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params)
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if resp.Content[0].Text != "from fallback" {
		t.Errorf("response = %q, want fallback", resp.Content[0].Text)
	}
	if got := string(fallback.lastReq[0].Model); got != "anthropic/claude-haiku-4.5" {
		t.Errorf("fallback model = %q, want catalog-mapped anthropic/claude-haiku-4.5", got)
	}
	if !c.Breaker(UpstreamAnthropic).Open() {
		t.Error("breaker must be open after a retryable primary failure")
	}

	// Breaker open → next call goes straight to the fallback (no probe yet).
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params); err != nil {
		t.Fatalf("SendParams (pinned): %v", err)
	}
	if primary.calls != 1 {
		t.Errorf("primary called %d times, want 1 (pinned open)", primary.calls)
	}
}

func TestSendParamsNonRetryableDoesNotFailOver(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(authErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params); err == nil {
		t.Fatal("401 must surface, not fail over")
	}
	if fallback.calls != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.calls)
	}
	if c.Breaker(UpstreamAnthropic).Open() {
		t.Error("401 must not trip the breaker")
	}
}

// An out-of-credit primary is not retryable, but it IS a reason to fail over:
// the account cannot answer any request until it is funded, so pinning callers
// to it would strand every send behind a billing problem.
func TestSendParamsFailsOverWhenPrimaryOutOfCredit(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(creditExhaustedErr()),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params)
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if resp.Content[0].Text != "from fallback" {
		t.Errorf("response = %q, want fallback", resp.Content[0].Text)
	}
	if !c.Breaker(UpstreamAnthropic).Open() {
		t.Error("breaker must be open after an out-of-credit primary rejection")
	}
}

// TestSendParamsSlashModelRoutesToOpenRouter proves an OpenRouter-native
// (slash) model — e.g. a resolved model alias like kimi-k3 — goes straight
// to the OpenRouter sender untranslated, never touching the Anthropic primary
// and never failing over (the caller asked for this specific model).
func TestSendParamsSlashModelRoutesToOpenRouter(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, openrouter),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	params := anthropic.MessageNewParams{Model: "moonshotai/kimi-k3", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params)
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if resp.Content[0].Text != "from openrouter" {
		t.Errorf("response = %q, want openrouter", resp.Content[0].Text)
	}
	if primary.calls != 0 {
		t.Errorf("anthropic primary called %d times, want 0", primary.calls)
	}
	if got := string(openrouter.lastReq[0].Model); got != "moonshotai/kimi-k3" {
		t.Errorf("openrouter model = %q, want moonshotai/kimi-k3 untranslated", got)
	}
}

// TestSendParamsPinnedModelCarriesProviderPrefs proves a provider-pinned
// slash model (routing provider pins) reaches the OpenRouter sender with the
// "provider" extra field set, and an unpinned one does not.
func TestSendParamsPinnedModelCarriesProviderPrefs(t *testing.T) {
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, &scriptedSender{}),
		WithUpstream(UpstreamOpenRouter, openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	send := func(model string) {
		t.Helper()
		params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 16,
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
		if _, err := c.SendParams(context.Background(), SendMeta{}, params); err != nil {
			t.Fatalf("SendParams(%s): %v", model, err)
		}
	}
	send("z-ai/glm-5.2")
	send("moonshotai/kimi-k3")

	wire := func(i int) string {
		t.Helper()
		b, err := json.Marshal(openrouter.lastReq[i])
		if err != nil {
			t.Fatalf("marshal wire params: %v", err)
		}
		return string(b)
	}
	if got := wire(0); !strings.Contains(got, `"provider":{"only":["fireworks"]}`) {
		t.Errorf("pinned model wire body missing provider pin: %s", got)
	}
	if got := wire(1); strings.Contains(got, `"provider"`) {
		t.Errorf("unpinned model must not carry a provider field: %s", got)
	}
}

// A slash model with no OpenRouter sender configured must fail cleanly, not
// leak the request to the Anthropic API (which would 404 the model anyway).
func TestSendParamsSlashModelWithoutOpenRouterErrors(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	params := anthropic.MessageNewParams{Model: "deepseek/deepseek-v4-pro", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	if _, err := c.SendParams(context.Background(), SendMeta{}, params); err == nil {
		t.Fatal("slash model without an OpenRouter sender must error")
	}
	if primary.calls != 0 {
		t.Errorf("anthropic primary called %d times, want 0", primary.calls)
	}
}

// TestSendParamsAnthropicPrefixRoutesNative proves the "anthropic/<x>" native
// marker reaches the direct Anthropic sender (prefix stripped on the wire),
// while a non-anthropic provider slash id still routes to OpenRouter — covering
// the second entry point where callers build params directly (bypassing
// ResolveModel).
func TestSendParamsAnthropicPrefixRoutesNative(t *testing.T) {
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from anthropic"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, anthropicSender),
		WithUpstream(UpstreamOpenRouter, openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}

	// "anthropic/" prefix -> native Anthropic sender, prefix stripped on the wire.
	params := anthropic.MessageNewParams{Model: "anthropic/sonnet-latest", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp, err := c.SendParams(context.Background(), SendMeta{}, params)
	if err != nil {
		t.Fatalf("SendParams(anthropic/...): %v", err)
	}
	if resp.Content[0].Text != "from anthropic" {
		t.Errorf("response = %q, want from anthropic", resp.Content[0].Text)
	}
	if openrouter.calls != 0 {
		t.Errorf("openrouter called %d times, want 0 (anthropic/ is native)", openrouter.calls)
	}
	if got := string(anthropicSender.lastReq[0].Model); got != "sonnet-latest" {
		t.Errorf("anthropic wire model = %q, want prefix-stripped sonnet-latest", got)
	}

	// A non-anthropic provider slash id still routes to OpenRouter, unchanged.
	params2 := anthropic.MessageNewParams{Model: "deepseek/deepseek-chat", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	resp2, err := c.SendParams(context.Background(), SendMeta{}, params2)
	if err != nil {
		t.Fatalf("SendParams(deepseek/...): %v", err)
	}
	if resp2.Content[0].Text != "from openrouter" {
		t.Errorf("response = %q, want from openrouter", resp2.Content[0].Text)
	}
	if got := string(openrouter.lastReq[0].Model); got != "deepseek/deepseek-chat" {
		t.Errorf("openrouter wire model = %q, want deepseek/deepseek-chat untranslated", got)
	}
	if anthropicSender.calls != 1 {
		t.Errorf("anthropic called %d times, want 1 (deepseek must not touch it)", anthropicSender.calls)
	}
}

// TestConversationAnthropicPrefixResolvesNative is the end-to-end check: an
// "anthropic/<family>-latest" model set on a conversation resolves (via
// ResolveModel at creation) to the concrete catalog id with the prefix gone,
// and the turn reaches the native Anthropic sender — never OpenRouter.
func TestConversationAnthropicPrefixResolvesNative(t *testing.T) {
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("native"),
	}}
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("openrouter"),
	}}
	c := newMemClient(t,
		WithUpstream(UpstreamAnthropic, anthropicSender),
		WithUpstream(UpstreamOpenRouter, openrouter),
		WithCatalog(seededCatalog(t)),
	)
	conv, err := c.Conversation(context.Background(),
		NewConversation("t", "test"), Model("anthropic/sonnet-latest"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if anthropicSender.calls != 1 {
		t.Errorf("anthropic called %d times, want 1", anthropicSender.calls)
	}
	if openrouter.calls != 0 {
		t.Errorf("openrouter called %d times, want 0 (anthropic/ resolves native)", openrouter.calls)
	}
	if got := string(anthropicSender.lastReq[0].Model); got != "claude-sonnet-5" {
		t.Errorf("resolved wire model = %q, want concrete claude-sonnet-5 (prefix stripped, alias pinned)", got)
	}
}

// TestConversationNoModelNoDefaultErrors proves the hardcoded haiku default is
// gone: with no per-conversation model and no WithDefaultModel, creation errors
// loudly rather than silently selecting a model.
func TestConversationNoModelNoDefaultErrors(t *testing.T) {
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, &scriptedSender{}),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Conversation(context.Background(), NewConversation("t", "test")); err == nil {
		t.Fatal("no per-conversation model + no default must error, not silently pick haiku")
	}
}

func TestSendParamsNoFallbackBypassesBreaker(t *testing.T) {
	primary := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(overloadedErr()), // trips via the WITH-fallback send
		respondText("direct despite pin"),
	}}
	fallback := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from fallback"),
	}}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	params := anthropic.MessageNewParams{Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}

	// Trip the breaker via a fallback-configured send.
	if _, err := c.SendParams(context.Background(), SendMeta{Fallback: []Upstream{UpstreamOpenRouter}}, params); err != nil {
		t.Fatalf("tripping send: %v", err)
	}
	if !c.Breaker(UpstreamAnthropic).Open() {
		t.Fatal("breaker should be open")
	}

	// A send with NO fallback opts out of pinning: direct primary despite the
	// open breaker (the per-conversation escape hatch from the design).
	resp, err := c.SendParams(context.Background(), SendMeta{}, params)
	if err != nil {
		t.Fatalf("no-fallback send: %v", err)
	}
	if resp.Content[0].Text != "direct despite pin" {
		t.Errorf("response = %q, want direct primary", resp.Content[0].Text)
	}
	if primary.calls != 2 {
		t.Errorf("primary calls = %d, want 2", primary.calls)
	}
}

func TestInMemoryConversation(t *testing.T) {
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("mem reply"),
		respondText("mem reply 2"),
	}}
	// No WithStore: conversation degrades to in-memory history.
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, sender),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(), NewConversation("brent", "cli"),
		Model("claude-haiku-4-5"), SystemText("sys"))
	if err != nil {
		t.Fatalf("store-less Conversation: %v", err)
	}
	if _, err := conv.Send(context.Background(), UserText("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("two")); err != nil {
		t.Fatal(err)
	}
	// History accumulated in memory: second request carried 3 messages.
	if n := len(sender.lastReq[1].Messages); n != 3 {
		t.Errorf("second request messages = %d, want 3", n)
	}
	history, err := conv.History(context.Background())
	if err != nil || len(history) != 4 {
		t.Errorf("history = %d err=%v, want 4", len(history), err)
	}
	// SeedHistory idempotence: re-seeding identical prefix no-ops.
	params := make([]Message, 0, 4)
	for _, m := range history {
		params = append(params, m.Param)
	}
	if err := conv.SeedHistory(context.Background(), params); err != nil {
		t.Errorf("idempotent re-seed failed: %v", err)
	}
	// Resume plumbing must refuse without a store.
	if _, err := conv.IncrementResumeAttempts(context.Background()); err == nil {
		t.Error("IncrementResumeAttempts must error without WithStore")
	}
}

func TestIsPromptTooLarge(t *testing.T) {
	if !isPromptTooLarge(promptTooLargeErr()) {
		t.Error("fabricated prompt-too-long error not recognized")
	}
	if isPromptTooLarge(authErr()) {
		t.Error("401 misclassified as prompt-too-large")
	}
	if isPromptTooLarge(nil) {
		t.Error("nil misclassified")
	}
}

func TestDefaultTrimPolicyKeepsFirstAndRecent(t *testing.T) {
	big := strings.Repeat("x", 60*1024)
	msgs := make([]Message, 0, 8)
	for range 8 {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(big)))
	}
	p := defaultTrimPolicy{}

	trimmed, ok := p.Trim(msgs, 0) // 300KB budget: first + ~4 recent fit
	if !ok {
		t.Fatal("trim must succeed on an oversized history")
	}
	if len(trimmed) >= len(msgs) {
		t.Fatalf("nothing trimmed: %d -> %d", len(msgs), len(trimmed))
	}
	if messageSize(trimmed[0]) != messageSize(msgs[0]) {
		t.Error("first message must be kept")
	}
	// The kept tail must be the MOST RECENT messages, in order.
	if messageSize(trimmed[len(trimmed)-1]) != messageSize(msgs[len(msgs)-1]) {
		t.Error("most recent message must be kept")
	}

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
	conv := &Conversation{cfg: convConfig{
		model:     "claude-haiku-4-5",
		maxTokens: 4096,
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
	if params.MaxTokens != 1024 {
		t.Errorf("per-send max_tokens override lost: %d", params.MaxTokens)
	}
	// Default policy: one 5m breakpoint on the LAST system block.
	var withCC int
	for i, b := range params.System {
		if b.CacheControl.Type != "" || b.CacheControl.TTL != "" {
			withCC++
			if i != len(params.System)-1 {
				t.Errorf("breakpoint on block %d, want last", i)
			}
			if b.CacheControl.TTL != "" {
				t.Errorf("TTL = %q, want empty (5m default)", b.CacheControl.TTL)
			}
		}
	}
	if withCC != 1 {
		t.Errorf("%d system cache breakpoints, want exactly 1", withCC)
	}
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
	if conv.cfg.system[len(conv.cfg.system)-1].CacheControl.Type != "" {
		t.Error("assemble mutated the conversation's system blocks")
	}
	if v, _ := params.Temperature.Value, false; v != 0.7 {
		t.Errorf("temperature = %v, want 0.7", v)
	}
}

func TestAssembleCachePolicyVariants(t *testing.T) {
	system := []anthropic.TextBlockParam{{Text: "sys"}}

	// 1h system, messages off.
	conv := &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: Cache1h, MessagesTTL: CacheOff, Breakpoints: 1}}}
	params := conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	if got := params.System[0].CacheControl.TTL; got != anthropic.CacheControlEphemeralTTLTTL1h {
		t.Errorf("system TTL = %q, want 1h", got)
	}
	mLast := params.Messages[0].Content
	if cc := mLast[len(mLast)-1].GetCacheControl(); cc != nil && cc.Type != "" {
		t.Error("messages off: unexpected moving breakpoint")
	}

	// Everything off.
	conv = &Conversation{cfg: convConfig{model: "m", system: system,
		cache: &CachePolicy{SystemTTL: CacheOff, MessagesTTL: CacheOff, Breakpoints: 1}}}
	params = conv.assemble(UserTextMessages("hi"), sendConfig{maxTokens: 64})
	if params.System[0].CacheControl.Type != "" {
		t.Error("system off: unexpected breakpoint")
	}
}

func TestWithMessageBreakpoints(t *testing.T) {
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
	if len(marked) != 2 {
		t.Fatalf("marked %v, want 2 breakpoints", marked)
	}
	if marked[len(marked)-1] != len(msgs)-1 {
		t.Errorf("last message not marked: %v", marked)
	}
	if gap := marked[1] - marked[0]; gap < lookbackStride {
		t.Errorf("breakpoint gap %d < stride %d", gap, lookbackStride)
	}
	// Copy-on-write: the input history carries no markers.
	if got := markedAt(msgs); got != nil {
		t.Errorf("input mutated: markers at %v", got)
	}

	// A history reloaded from capture carries stale markers verbatim; they
	// must be cleared on the assembled request (4-breakpoint API limit).
	stale := withMessageBreakpoints(out, policy) // out has markers baked in
	if got := markedAt(stale); len(got) != 2 {
		t.Errorf("stale markers not consolidated: %v", got)
	}

	// One more message MOVES the markers on the new request.
	grown := append(append([]Message{}, msgs...), anthropic.NewUserMessage(anthropic.NewTextBlock("new")))
	out2 := withMessageBreakpoints(grown, policy)
	m2 := markedAt(out2)
	if len(m2) != 2 || m2[len(m2)-1] != len(grown)-1 {
		t.Errorf("after growth: markers %v, want last=%d", m2, len(grown)-1)
	}

	// Off policy still scrubs stale markers.
	off := withMessageBreakpoints(out, &CachePolicy{MessagesTTL: CacheOff, Breakpoints: 1})
	if got := markedAt(off); got != nil {
		t.Errorf("off policy left markers: %v", got)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrimaryOptionRoutesUpstream(t *testing.T) {
	anthropicSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from anthropic"),
	}}
	openrouterSender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("from openrouter"),
	}}
	c := newMemClient(t,
		WithUpstream(UpstreamAnthropic, anthropicSender),
		WithUpstream(UpstreamOpenRouter, openrouterSender),
	)
	conv, err := c.Conversation(context.Background(),
		NewConversation("t", "test"), Model("claude-test"), Primary(UpstreamOpenRouter))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if openrouterSender.calls != 1 {
		t.Fatalf("openrouter (declared primary) called %d times, want 1", openrouterSender.calls)
	}
	if anthropicSender.calls != 0 {
		t.Fatalf("anthropic called %d times, want 0 (openrouter is primary)", anthropicSender.calls)
	}
}

func TestThinkingBudgetSetsParam(t *testing.T) {
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := newMemClient(t, WithUpstream(UpstreamAnthropic, sender))
	conv, err := c.Conversation(context.Background(),
		NewConversation("t", "test"), Model("claude-test"), ThinkingBudget(8192))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if len(sender.lastReq) == 0 {
		t.Fatal("sender captured no request")
	}
	last := sender.lastReq[len(sender.lastReq)-1]
	if last.Thinking.OfEnabled == nil || last.Thinking.OfEnabled.BudgetTokens != 8192 {
		t.Fatalf("thinking budget not set: %+v", last.Thinking)
	}
}
