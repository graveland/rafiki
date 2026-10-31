// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"
)

// Sender issues one model call in rafiki's native types. Every first-party
// sender implements both methods; Stream's decoder must be closed by the caller.
type Sender interface {
	Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error)
	Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error)
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

// ErrNeedsOwnerSender marks the one refusal SenderForKey makes that is not a
// fault: a kind that authenticates as an OWNER (openai-responses-chatgpt) has
// no key-based sender by design, so its sender is bound per owner at spawn
// instead. Callers that only warn about unusable providers can tell this apart
// from a genuinely broken entry with errors.Is and stay quiet.
var ErrNeedsOwnerSender = errors.New("llm: needs an owner-bound sender")

// SenderFor builds a Sender for a provider, resolving its credential from the
// environment (empty for a keyless provider).
func SenderFor(p providers.Provider, rt http.RoundTripper) (Sender, error) {
	return SenderForKey(p, p.APIKey(), rt)
}

// SenderForKey is SenderFor with an explicit credential, for the per-spawn key
// a caller forwards (SpawnRequest.APIKey) which the daemon's environment does
// not hold. An empty key means keyless: no credential header is sent at all.
func SenderForKey(p providers.Provider, key string, rt http.RoundTripper) (Sender, error) {
	switch p.Kind {
	case providers.KindAnthropic:
		base := p.BaseURL
		if base == "" {
			base = anthropicBaseURL
		}
		return newAnthropicSender(p, base, key, rt)
	case providers.KindAnthropicOpenRouter:
		base := p.BaseURL
		if base == "" {
			base = openRouterBaseURL
		}
		return newOpenRouterSender(p, base, key, rt)
	case providers.KindOpenAI:
		return newOpenAISender(p, key, rt)
	case providers.KindOpenAIResponses:
		return newResponsesSender(p, key, rt)
	case providers.KindOpenAIResponsesChatGPT:
		// The ChatGPT dialect authenticates as an OWNER, from a credential the
		// daemon holds; there is no key to build it from, and inventing one
		// (an env var, a daemon-wide login) is exactly what the kind exists to
		// prevent. NewChatGPTResponsesSender is the only constructor.
		return nil, fmt.Errorf("%w: provider %q: kind %q uses NewChatGPTResponsesSender and cannot be built from a key", ErrNeedsOwnerSender, p.Name, p.Kind)
	default:
		return nil, fmt.Errorf("llm: provider %q: unknown kind %q", p.Name, p.Kind)
	}
}
