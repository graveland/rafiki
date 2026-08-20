// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
)

type recordingSender struct {
	name  string
	calls *[]string
	// err must be FAILOVER-WORTHY for the fallback tests to reach the fallback.
	// routing.FailoverWorthy explicitly excludes context.Canceled and
	// context.DeadlineExceeded (pkg/routing/classify.go), so a deadline error
	// here would make callModel return immediately and the test would assert
	// the wrong thing. A 5xx *anthropic.Error is the reliable choice.
	err error
}

func failoverWorthyErr() error {
	resp := &http.Response{StatusCode: 503, Status: "503 Service Unavailable"}
	req, _ := http.NewRequest("POST", "http://x", nil)
	return &anthropic.Error{StatusCode: 503, Request: req, Response: resp}
}

func (r recordingSender) New(_ context.Context, p anthropic.MessageNewParams) (*anthropic.Message, error) {
	*r.calls = append(*r.calls, r.name+":"+string(p.Model))
	if r.err != nil {
		return nil, r.err
	}
	return &anthropic.Message{}, nil
}

func localSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(`
default_provider = "vmlx"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return set
}

// The old NewClient hard-required an UpstreamAnthropic sender, which made a
// keyless local-only client impossible to construct. That requirement is gone:
// what is required is a sender for the provider the model names.
func TestNewClientNeedsNoAnthropicSender(t *testing.T) {
	var calls []string
	c, err := llm.NewClient(
		llm.WithProviders(localSet(t)),
		llm.WithProviderSender("vmlx", recordingSender{name: "vmlx", calls: &calls}),
		llm.WithDefaultModel("vmlx/qwen3"),
	)
	if err != nil {
		t.Fatalf("NewClient with no anthropic sender: %v", err)
	}
	if _, err := c.SendParams(context.Background(), llm.SendMeta{}, anthropic.MessageNewParams{
		Model: anthropic.Model("vmlx/qwen3"), MaxTokens: 8,
	}); err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	// The sender receives the PROVIDER-LOCAL id, with the provider prefix
	// stripped: "vmlx/qwen3" addresses provider vmlx and model qwen3.
	if len(calls) != 1 || calls[0] != "vmlx:qwen3" {
		t.Errorf("calls = %v, want [vmlx:qwen3]", calls)
	}
}

func TestSendParamsUnknownProviderErrors(t *testing.T) {
	c, err := llm.NewClient(
		llm.WithProviders(localSet(t)),
		llm.WithProviderSender("vmlx", recordingSender{name: "vmlx", calls: &[]string{}}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.SendParams(context.Background(), llm.SendMeta{}, anthropic.MessageNewParams{
		Model: anthropic.Model("deepseek/deepseek-chat"), MaxTokens: 8,
	})
	if err == nil {
		t.Fatal("SendParams with an unconfigured provider succeeded")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %q, want \"unknown provider\"", err.Error())
	}
}

// Configured fallback applies when the caller supplies none.
func TestConfiguredFallbackUsed(t *testing.T) {
	set, err := providers.Parse([]byte(`
default_provider = "primary"

[providers.primary]
kind = "anthropic"
base_url = "http://localhost:1"
fallback = ["backup"]

[providers.backup]
kind = "anthropic"
base_url = "http://localhost:2"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var calls []string
	c, err := llm.NewClient(
		llm.WithProviders(set),
		llm.WithProviderSender("primary", recordingSender{name: "primary", calls: &calls, err: failoverWorthyErr()}),
		llm.WithProviderSender("backup", recordingSender{name: "backup", calls: &calls}),
		llm.WithBreaker(time.Minute),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.SendParams(context.Background(), llm.SendMeta{}, anthropic.MessageNewParams{
		Model: anthropic.Model("primary/m"), MaxTokens: 8,
	}); err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary:m" || calls[1] != "backup:m" {
		t.Errorf("calls = %v, want [primary:m backup:m]", calls)
	}
}

// A caller-supplied fallback list wins outright, and an EMPTY caller list means
// no failover — nothing silently substitutes the configured list for it.
func TestCallerFallbackWins(t *testing.T) {
	set, err := providers.Parse([]byte(`
default_provider = "primary"

[providers.primary]
kind = "anthropic"
base_url = "http://localhost:1"
fallback = ["backup"]

[providers.backup]
kind = "anthropic"
base_url = "http://localhost:2"

[providers.other]
kind = "anthropic"
base_url = "http://localhost:3"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var calls []string
	c, err := llm.NewClient(
		llm.WithProviders(set),
		llm.WithProviderSender("primary", recordingSender{name: "primary", calls: &calls, err: failoverWorthyErr()}),
		llm.WithProviderSender("backup", recordingSender{name: "backup", calls: &calls}),
		llm.WithProviderSender("other", recordingSender{name: "other", calls: &calls}),
		llm.WithBreaker(time.Minute),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.SendParams(context.Background(), llm.SendMeta{Fallback: []string{"other"}}, anthropic.MessageNewParams{
		Model: anthropic.Model("primary/m"), MaxTokens: 8,
	}); err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if len(calls) != 2 || calls[1] != "other:m" {
		t.Errorf("calls = %v, want the caller's [other] to win over the configured [backup]", calls)
	}
}
