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
)

// A local, keyless Anthropic-compatible server is the whole point of the
// feature: the request must carry NO credential header at all, and must reach
// the configured base_url rather than api.anthropic.com.
// ANTHROPIC_API_KEY must be cleared because the SDK's DefaultClientOptions
// reads it and applies it as an implicit WithAPIKey even when no option is
// passed.
func TestSenderForKeylessLocal(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
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
	if err != nil {
		t.Fatalf("SenderFor: %v", err)
	}
	if _, err := sender.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model("local"),
		MaxTokens: 16,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "" {
		t.Errorf("x-api-key = %q, want empty: a keyless provider must send no credential", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}

func TestSenderForSendsKeyWhenConfigured(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-test-123")
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
	if err != nil {
		t.Fatalf("SenderFor: %v", err)
	}
	_, _ = sender.New(context.Background(), anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if gotKey != "sk-test-123" {
		t.Errorf("x-api-key = %q, want sk-test-123", gotKey)
	}
}

// The openrouter kind's headers are owned by the handler, never by config.
func TestSenderForOpenRouterHeaders(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "sk-or-1")
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
	if err != nil {
		t.Fatalf("SenderFor: %v", err)
	}
	_, _ = sender.New(context.Background(), anthropic.MessageNewParams{
		Model: anthropic.Model("m"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if referer == "" || title != "rafiki" {
		t.Errorf("Referer = %q, X-OpenRouter-Title = %q; want the handler's own headers", referer, title)
	}
}

// The kind is reserved, not implemented. Constructing it must fail with a
// message that says so, not produce a sender that 400s at call time.
func TestSenderForOpenAIKindRefused(t *testing.T) {
	_, err := llm.SenderFor(providers.Provider{Name: "x", Kind: providers.KindOpenAI, BaseURL: "http://x"}, nil)
	if err == nil {
		t.Fatal("SenderFor(openai) succeeded; the kind is reserved and unimplemented")
	}
}

func TestSenderForStreams(t *testing.T) {
	sender, err := llm.SenderFor(providers.Provider{Name: "x", Kind: providers.KindAnthropic, BaseURL: "http://127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatalf("SenderFor: %v", err)
	}
	if _, ok := sender.(llm.StreamingSender); !ok {
		t.Error("SenderFor must return a StreamingSender; the streaming path silently degrades to non-streaming otherwise")
	}
}
