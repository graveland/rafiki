// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/providers"
)

// Sender issues one Messages-API call. The SDK client is wrapped behind this
// seam so tests inject fakes and the client stays provider-agnostic.
type Sender interface {
	New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

// StreamingSender is an optional capability: a Sender that can also open a
// streaming Messages call. Callers type-assert for it and fall back to New,
// so a Sender that cannot stream (test fakes, custom transports) stays valid.
// Kept separate from Sender so this remains an additive change upstream.
// The error return exists for implementations other than sdkSender: the
// Anthropic SDK's own Messages.NewStreaming never fails this way (see
// sdkSender.NewStreaming below), so no first-party sender ever returns a
// non-nil error here. A StreamingSender that does MAY also return a non-nil
// stream alongside it — sendStreaming's caller is responsible for closing
// that stream on every path, including this one, so implementations should
// not assume an error return means nothing needs cleanup.
type StreamingSender interface {
	Sender
	NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error)
}

// sessionIDContextKey is the context key WithSessionID writes and
// sessionIDTransport reads. Unexported: nothing outside this file may set or
// read it directly, so the only way a request carries a session id is
// through WithSessionID.
type sessionIDContextKey struct{}

// WithSessionID attaches a stable per-conversation identifier to ctx, sent as
// OpenRouter's "x-session-id" header on every request issued with it (via
// sessionIDTransport, wired onto anthropic-openrouter senders in
// SenderForKey). This is the same sticky-routing header the reverse-proxy
// face already sends for passthrough clients (pkg/server/proxy.go,
// pkg/server/openai.go); without it, OpenRouter falls back to hashing the
// first system+user message pair for routing, which pins only a
// conversation's static prefix and leaves every later turn's growing tail
// unrouted — see the ProviderGuard notes on cache locality. An empty id is a
// no-op: ctx is returned unchanged and no header is ever sent.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDContextKey{}, id)
}

// sessionIDTransport sets x-session-id from ctx (via WithSessionID) on every
// outbound request, so OpenRouter can pin a whole conversation's requests to
// one backend for prompt-cache locality. Only ever wrapped around an
// anthropic-openrouter sender's transport in SenderForKey — the
// Anthropic-native path must never see this header.
type sessionIDTransport struct {
	base http.RoundTripper
}

func (t sessionIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if id, ok := req.Context().Value(sessionIDContextKey{}).(string); ok && id != "" {
		// Clone rather than mutate: RoundTrip must not modify the original
		// request (net/http.RoundTripper's contract).
		req = req.Clone(req.Context())
		req.Header.Set("x-session-id", id)
	}
	return base.RoundTrip(req)
}

type sdkSender struct{ client anthropic.Client }

func (s sdkSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	return s.client.Messages.New(ctx, params)
}

// NewStreaming opens a streaming Messages call. The SDK's NewStreaming does
// not itself return an error (errors surface as an error event on the stream
// or from the stream's Err() after iteration), so the nil here is not a
// stub — it matches the real signature.
func (s sdkSender) NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	return s.client.Messages.NewStreaming(ctx, params), nil
}

// FromSDK wraps a pre-built SDK client as a Sender — e.g. sc's forward-mode
// diagnose client pointed at the central /v1/messages proxy.
func FromSDK(client anthropic.Client) Sender {
	return sdkSender{client: client}
}

// SenderFor builds a Sender for a provider, resolving its credential from the
// environment (empty for a keyless provider).
func SenderFor(p providers.Provider, rt http.RoundTripper) (Sender, error) {
	return SenderForKey(p, p.APIKey(), rt)
}

// SenderForKey is SenderFor with an explicit credential, for the per-spawn key
// a caller forwards (SpawnRequest.APIKey) which the daemon's environment does
// not hold. An empty key means keyless: no x-api-key header is sent at all.
func SenderForKey(p providers.Provider, key string, rt http.RoundTripper) (Sender, error) {
	opts := []option.RequestOption{}

	base := p.BaseURL
	switch p.Kind {
	case providers.KindAnthropic:
		if base == "" {
			base = anthropicBaseURL
		}
	case providers.KindAnthropicOpenRouter:
		if base == "" {
			base = openRouterBaseURL
		}
		opts = append(opts,
			option.WithHeader("Referer", "https://github.com/graveland/rafiki"),
			option.WithHeader("X-OpenRouter-Title", "rafiki"),
			option.WithHeader("X-OpenRouter-Categories", "cli-agent"),
		)
		rt = sessionIDTransport{base: rt}
	case providers.KindOpenAI:
		return nil, fmt.Errorf("llm: provider %q: kind %q is reserved and not implemented", p.Name, p.Kind)
	default:
		return nil, fmt.Errorf("llm: provider %q: unknown kind %q", p.Name, p.Kind)
	}
	opts = append(opts, option.WithBaseURL(base))

	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	if rt != nil {
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: rt}))
	}
	return sdkSender{client: anthropic.NewClient(opts...)}, nil
}
