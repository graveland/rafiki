// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

type recordingSender struct {
	name  string
	calls *[]string
	// err must be FAILOVER-WORTHY for the fallback tests to reach the fallback.
	// routing.FailoverWorthy explicitly excludes context.Canceled and
	// context.DeadlineExceeded (pkg/routing/classify.go), so a deadline error
	// here would make callModel return immediately and the test would assert
	// the wrong thing. A 5xx *msg.APIError is the reliable choice.
	err error
}

func failoverWorthyErr() error {
	return &msg.APIError{Status: http.StatusServiceUnavailable, Kind: msg.ErrOther, Dialect: msg.DialectAnthropic}
}

func (r recordingSender) Send(_ context.Context, req *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	*r.calls = append(*r.calls, r.name+":"+req.Model)
	if r.err != nil {
		return nil, r.err
	}
	return &msg.Response{}, nil
}

func (r recordingSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := r.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return llmtest.NewDecoder(llmtest.Events(resp)), nil
}

func localSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(`
default_provider = "vmlx"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"
`))
	assert.NewAborting(t).NoError(err, "Parse")
	return set
}

// The old NewClient hard-required an UpstreamAnthropic sender, which made a
// keyless local-only client impossible to construct. That requirement is gone:
// what is required is a sender for the provider the model names.
func TestNewClientNeedsNoAnthropicSender(t *testing.T) {
	ck := assert.NewCollecting(t)
	var calls []string
	c, err := NewClient(
		WithProviders(localSet(t)),
		WithProviderSender("vmlx", recordingSender{name: "vmlx", calls: &calls}),
		WithDefaultModel("vmlx/qwen3"),
	)
	ck.Require().NoError(err, "NewClient with no anthropic sender")
	if _, err := c.send(context.Background(), SendMeta{}, &msg.Request{Model: "vmlx/qwen3", MaxTokens: 8}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The sender receives the PROVIDER-LOCAL id, with the provider prefix
	// stripped: "vmlx/qwen3" addresses provider vmlx and model qwen3.
	ck.False(len(calls) != 1 || calls[0] != "vmlx:qwen3", "calls = %v, want [vmlx:qwen3]", calls)
}

func TestSendUnknownProviderErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, err := NewClient(
		WithProviders(localSet(t)),
		WithProviderSender("vmlx", recordingSender{name: "vmlx", calls: &[]string{}}),
	)
	ck.Require().NoError(err, "NewClient")
	_, err = c.send(context.Background(), SendMeta{}, &msg.Request{Model: "deepseek/deepseek-chat", MaxTokens: 8}, nil)
	ck.Require().Error(err, "send with an unconfigured provider succeeded")
	ck.StrContains(err.Error(), "unknown provider", "error = %q, want \"unknown provider\"", err.Error())
}

// Configured fallback applies when the caller supplies none.
func TestConfiguredFallbackUsed(t *testing.T) {
	ck := assert.NewCollecting(t)
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
	ck.Require().NoError(err, "Parse")
	var calls []string
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("primary", recordingSender{name: "primary", calls: &calls, err: failoverWorthyErr()}),
		WithProviderSender("backup", recordingSender{name: "backup", calls: &calls}),
		WithBreaker(time.Minute),
	)
	ck.Require().NoError(err, "NewClient")
	if _, err := c.send(context.Background(), SendMeta{}, &msg.Request{Model: "primary/m", MaxTokens: 8}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	ck.False(len(calls) != 2 || calls[0] != "primary:m" || calls[1] != "backup:m", "calls = %v, want [primary:m backup:m]", calls)
}

// A caller-supplied fallback list wins outright, and an EMPTY caller list means
// no failover — nothing silently substitutes the configured list for it.
func TestCallerFallbackWins(t *testing.T) {
	ck := assert.NewCollecting(t)
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
	ck.Require().NoError(err, "Parse")
	var calls []string
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("primary", recordingSender{name: "primary", calls: &calls, err: failoverWorthyErr()}),
		WithProviderSender("backup", recordingSender{name: "backup", calls: &calls}),
		WithProviderSender("other", recordingSender{name: "other", calls: &calls}),
		WithBreaker(time.Minute),
	)
	ck.Require().NoError(err, "NewClient")
	if _, err := c.send(context.Background(), SendMeta{Fallback: []string{"other"}}, &msg.Request{Model: "primary/m", MaxTokens: 8}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	ck.False(len(calls) != 2 || calls[1] != "other:m", "calls = %v, want the caller's [other] to win over the configured [backup]", calls)
}
