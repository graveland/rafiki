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
// sessionIDFromContext reads. Unexported: nothing outside this file may set
// or read it directly, so the only way a request carries a session id is
// through WithSessionID.
type sessionIDContextKey struct{}

// WithSessionID attaches a stable per-conversation identifier to ctx, sent on
// whatever header a provider declares via providers.toml's session_header
// (SenderForKey wires sessionIDTransport, or for the openai kind, the
// sender's own request builders — see sessionIDFromContext) — OpenRouter's is
// "x-session-id". This is the same sticky-routing header the reverse-proxy
// face already sends for passthrough clients (pkg/server/proxy.go,
// pkg/server/openai.go); without it, a provider with no other signal falls
// back to hashing the first system+user message pair for routing, which pins
// only a conversation's static prefix and leaves every later turn's growing
// tail unrouted — see the ProviderGuard notes on cache locality. An empty id
// is a no-op: ctx is returned unchanged and no header is ever sent. A
// provider with no session_header configured also never sends one,
// regardless of what ctx carries.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDContextKey{}, id)
}

// sessionIDTransport sets header from ctx (via WithSessionID) on every
// outbound request, so a provider that supports sticky routing can pin a
// whole conversation's requests to one backend for prompt-cache locality.
// header is provider-specific — OpenRouter's is "x-session-id" — and is
// wired only when the provider declares a non-empty SessionHeader in
// providers.toml (SenderForKey). The Anthropic-native path must never see
// this unless it opts in.
type sessionIDTransport struct {
	base   http.RoundTripper
	header string
}

func (t sessionIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if id, ok := sessionIDFromContext(req.Context()); ok && t.header != "" {
		// Clone rather than mutate: RoundTrip must not modify the original
		// request (net/http.RoundTripper's contract).
		req = req.Clone(req.Context())
		req.Header.Set(t.header, id)
	}
	return base.RoundTrip(req)
}

// sessionIDFromContext reads the identifier WithSessionID attached, if any.
// Shared by sessionIDTransport and the openai kind's own request builders
// (which set headers directly rather than through a RoundTripper), so both
// paths read the identical ctx value the same way.
func sessionIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(sessionIDContextKey{}).(string)
	return id, ok && id != ""
}

// rawTraceHeaderKey is the context key rawTraceHeaders (in client.go) and
// headerCaptureTransport share: withRawTraceHeaders attaches the pointer,
// headerCaptureTransport fills it in from the real request actually placed on
// the wire. Unexported: nothing outside this package may set or read it.
type rawTraceHeaderKey struct{}

// rawTraceHeaders holds the real request/response headers for one HTTP
// attempt, captured below the SDK so recordRawTrace never has to guess at
// what was actually sent. Both fields stay nil (not just empty) when no
// attempt reached the transport, matching nilJSON's empty-input-is-NULL rule.
type rawTraceHeaders struct {
	req  http.Header
	resp http.Header
}

// withRawTraceHeaders attaches a fresh capture point to ctx and returns it
// alongside. Call once per logical Send/stream attempt, before the context
// reaches any sender — every RoundTrip made with a descendant of the returned
// ctx overwrites the pointed-to struct, so on a fallback chain the struct
// ends up holding the LAST attempt's headers, which is exactly the attempt
// whose (resp, err) the caller goes on to record.
func withRawTraceHeaders(ctx context.Context) (context.Context, *rawTraceHeaders) {
	h := &rawTraceHeaders{}
	return context.WithValue(ctx, rawTraceHeaderKey{}, h), h
}

// headerCaptureTransport records the real outbound request headers and real
// inbound response headers of every call, so raw-trace capture reflects what
// actually went over the wire instead of a hand-maintained guess at which
// headers matter. Wrapped around every sender's transport in SenderForKey,
// unconditionally: recordRawTrace redacts credentials before persisting, so
// capturing them here (to redact, not to store) is not itself a leak.
type headerCaptureTransport struct{ base http.RoundTripper }

func (t headerCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if h, ok := req.Context().Value(rawTraceHeaderKey{}).(*rawTraceHeaders); ok && h != nil {
		h.req = req.Header.Clone()
		if resp != nil {
			h.resp = resp.Header.Clone()
		}
	}
	return resp, err
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
	sessionHeader := p.SessionHeader
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
		// "x-session-id" is OpenRouter's implicit default, unconditional
		// before session_header was configurable — preserved here so an
		// ad-hoc Provider{Kind: KindAnthropicOpenRouter} literal (any code
		// that doesn't go through providers.Default() or a providers.toml
		// entry naming session_header explicitly) keeps working exactly as
		// before. An explicit session_header always wins over this default.
		if sessionHeader == "" {
			sessionHeader = "x-session-id"
		}
	case providers.KindOpenAI:
		return newOpenAISender(p, key, rt)
	default:
		return nil, fmt.Errorf("llm: provider %q: unknown kind %q", p.Name, p.Kind)
	}
	opts = append(opts, option.WithBaseURL(base))

	// Opt-in per provider, not tied to Kind: a plain KindAnthropic entry
	// (e.g. Fireworks' Anthropic-compatible endpoint) wants this exactly as
	// much as KindAnthropicOpenRouter does when it declares session_header —
	// the header NAME is provider-specific, the need for sticky routing is
	// not. KindAnthropic has no implicit default (unlike OpenRouter above),
	// so it stays silent unless session_header is set.
	if sessionHeader != "" {
		rt = sessionIDTransport{base: rt, header: sessionHeader}
	}

	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	// Unconditional, and last: every sender's transport must be capturable
	// regardless of whether a caller passed one, or fundi's raw-trace capture
	// silently sees nothing for a provider that had no custom rt.
	opts = append(opts, option.WithHTTPClient(&http.Client{Transport: headerCaptureTransport{base: rt}}))
	return sdkSender{client: anthropic.NewClient(opts...)}, nil
}
