// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// A local, keyless Anthropic-compatible server is the whole point of the
// feature: the request must carry NO credential header at all, and must reach
// the configured base_url rather than api.anthropic.com.
// ANTHROPIC_API_KEY must be cleared because the SDK's DefaultClientOptions
// reads it and applies it as an implicit WithAPIKey even when no option is
// passed.
func TestSenderForKeylessLocal(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	c := assert.NewCollecting(t)
	var gotPath, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"local","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name:    "vmlx",
		Kind:    providers.KindAnthropic,
		BaseURL: srv.URL,
	}, nil)
	c.Require().NoError(err, "SenderFor")
	if _, err := sender.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model("local"),
		MaxTokens: 16,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Eq("/v1/messages", gotPath, "path")
	c.Eq("", gotKey, "x-api-key")
	c.Eq("", gotAuth, "Authorization")
}

func TestSenderForSendsKeyWhenConfigured(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-test-123")
	c := assert.NewCollecting(t)
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name:      "keyed",
		Kind:      providers.KindAnthropic,
		BaseURL:   srv.URL,
		APIKeyEnv: "TEST_PROVIDER_KEY",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	_, _ = sender.New(context.Background(), anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.Eq("sk-test-123", gotKey, "x-api-key")
}

// The openrouter kind's headers are owned by the handler, never by config.
func TestSenderForOpenRouterHeaders(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "sk-or-1")
	c := assert.NewCollecting(t)
	var referer, title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer = r.Header.Get("Referer")
		title = r.Header.Get("X-OpenRouter-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "openrouter", Kind: providers.KindAnthropicOpenRouter,
		BaseURL: srv.URL, APIKeyEnv: "TEST_OR_KEY",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	_, _ = sender.New(context.Background(), anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.False(referer == "" || title != "rafiki", "Referer = %q, X-OpenRouter-Title = %q; want the handler's own headers", referer, title)
}

// WithSessionID must reach OpenRouter as x-session-id — the sticky-routing
// header that pins a whole conversation's requests to the same backend for
// prompt-cache locality (see pkg/server/proxy.go's identical header on the
// passthrough face; this is the same mechanism for fundi's native path).
func TestSenderForOpenRouterSessionID(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "sk-or-1")
	c := assert.NewCollecting(t)
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "openrouter", Kind: providers.KindAnthropicOpenRouter,
		BaseURL: srv.URL, APIKeyEnv: "TEST_OR_KEY",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	ctx := llm.WithSessionID(context.Background(), "conv-abc-123")
	_, _ = sender.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.Eq("conv-abc-123", gotSession, "x-session-id")
}

// No WithSessionID on the context means no header at all — never an empty
// x-session-id value, which OpenRouter would treat as a real (empty) session.
func TestSenderForOpenRouterNoSessionIDWithoutContextValue(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "sk-or-1")
	c := assert.NewCollecting(t)
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["X-Session-Id"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "openrouter", Kind: providers.KindAnthropicOpenRouter,
		BaseURL: srv.URL, APIKeyEnv: "TEST_OR_KEY",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	_, _ = sender.New(context.Background(), anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.False(sawHeader, "x-session-id header present with no session id on the context")
}

// The Anthropic-native path must never see x-session-id, even if the caller's
// context happens to carry one (e.g. a fallback chain sharing ctx with an
// OpenRouter primary) — it is an OpenRouter-only concept.
func TestSenderForAnthropicNeverSeesSessionID(t *testing.T) {
	c := assert.NewCollecting(t)
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["X-Session-Id"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "anthropic", Kind: providers.KindAnthropic, BaseURL: srv.URL,
	}, nil)
	c.Require().NoError(err, "SenderFor")
	ctx := llm.WithSessionID(context.Background(), "conv-abc-123")
	_, _ = sender.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.False(sawHeader, "x-session-id header present on the Anthropic-native path")
}

// The kind is implemented (Task 2.1): SenderForKey routes it through
// newOpenAISender and must hand back a StreamingSender, like the other kinds.
// The constructor's empty-BaseURL-is-a-config-error contract is pinned in
// openai_sender_test.go (TestOpenAISenderNewRequiresBaseURL,
// TestSenderForKeyBuildsOpenAISender).
func TestSenderForKeyOpenAIKindBuildsSender(t *testing.T) {
	c := assert.NewCollecting(t)
	s, err := llm.SenderFor(providers.Provider{Name: "x", Kind: providers.KindOpenAI, BaseURL: "http://x"}, nil)
	c.Require().NoError(err, "SenderFor(openai)")
	_, ok := s.(llm.StreamingSender)
	c.True(ok, "SenderFor(openai) must return a StreamingSender; streaming would silently degrade to non-streaming")
}

func TestSenderForStreams(t *testing.T) {
	c := assert.NewCollecting(t)
	sender, err := llm.SenderFor(providers.Provider{Name: "x", Kind: providers.KindAnthropic, BaseURL: "http://127.0.0.1:1"}, nil)
	c.Require().NoError(err, "SenderFor")
	_, ok := sender.(llm.StreamingSender)
	c.True(ok, "SenderFor must return a StreamingSender; the streaming path silently degrades to non-streaming otherwise")
}

// A KindAnthropic provider (e.g. Fireworks' Anthropic-compatible endpoint)
// has no implicit session header — TestSenderForAnthropicNeverSeesSessionID
// pins that — but must send whatever session_header it declares, on the name
// it declares, not OpenRouter's "x-session-id". This is the actual new
// capability: pinning conversations to a backend on a provider whose sticky
// routing header isn't OpenRouter's.
func TestSenderForAnthropicKindHonorsConfiguredSessionHeader(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotAffinity string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAffinity = r.Header.Get("x-session-affinity")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "fireworks", Kind: providers.KindAnthropic,
		BaseURL: srv.URL, SessionHeader: "x-session-affinity",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	ctx := llm.WithSessionID(context.Background(), "conv-xyz-789")
	_, _ = sender.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.Eq("conv-xyz-789", gotAffinity, "x-session-affinity")
}

// An explicit session_header on a KindAnthropicOpenRouter provider overrides
// the "x-session-id" implicit default rather than sending both or being
// ignored — the override, not the default, must win.
func TestSenderForOpenRouterExplicitSessionHeaderOverridesDefault(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "sk-or-1")
	c := assert.NewCollecting(t)
	var gotCustom, gotDefault string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("x-custom-session")
		gotDefault = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	sender, err := llm.SenderFor(providers.Provider{
		Name: "openrouter", Kind: providers.KindAnthropicOpenRouter,
		BaseURL: srv.URL, APIKeyEnv: "TEST_OR_KEY", SessionHeader: "x-custom-session",
	}, nil)
	c.Require().NoError(err, "SenderFor")
	ctx := llm.WithSessionID(context.Background(), "conv-abc-123")
	_, _ = sender.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.Eq("conv-abc-123", gotCustom, "x-custom-session")
	c.Eq("", gotDefault, "x-session-id")
}
