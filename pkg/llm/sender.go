// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"

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

// rawTraceHeaders holds what the transport saw for one HTTP attempt, captured
// below the sender so recordRawTrace never has to guess at what was actually
// sent. Every field stays zero when no attempt reached the transport, matching
// nilJSON's empty-input-is-NULL rule.
type rawTraceHeaders struct {
	// req and resp are the real wire headers. Their Content-Encoding and
	// Content-Length describe the TRANSPORT (the compressed body), not the
	// decompressed body stored in reqBody.
	req  http.Header
	resp http.Header
	// reqBody is the request body decompressed to the dialect's actual JSON,
	// bounded by maxTraceBody. It is nil when the body could not be read or
	// decoded — never the compressed bytes, which no reader could use.
	reqBody []byte
	// reqPath is the request URL path the transport saw.
	reqPath string
	// captureErr explains a missing or truncated reqBody. It is a diagnostic
	// recorded beside the call, never an error of the call.
	captureErr string
}

// maxTraceBody bounds the decompressed body a raw trace stores. A request body
// is bounded in practice, and a limit is what keeps a hostile or broken
// Content-Encoding from expanding without end.
const maxTraceBody = 8 << 20

// withRawTraceHeaders attaches a fresh capture point to ctx and returns it
// alongside. Call once per logical Send/stream attempt, before the context
// reaches any sender — every RoundTrip made with a descendant of the returned
// ctx overwrites the pointed-to struct, so on a fallback chain the struct
// ends up holding the LAST attempt's headers, path and decompressed body,
// which is exactly the attempt whose (resp, err) the caller goes on to record.
func withRawTraceHeaders(ctx context.Context) (context.Context, *rawTraceHeaders) {
	h := &rawTraceHeaders{}
	return context.WithValue(ctx, rawTraceHeaderKey{}, h), h
}

// rawTraceContext attaches the capture point only when raw tracing is on. With
// it off nothing will be recorded, so attaching one would make every request
// pay a body read (and, for a compressed dialect, a decompression) for a record
// nobody writes.
func (c *Client) rawTraceContext(ctx context.Context) (context.Context, *rawTraceHeaders) {
	if c.rawTrace == nil {
		return ctx, nil
	}
	return withRawTraceHeaders(ctx)
}

// headerCaptureTransport records the real outbound request headers, the
// outbound body (decompressed), the request path and the real inbound response
// headers of every call, so raw-trace capture reflects what actually went over
// the wire instead of a hand-maintained guess at which headers matter. Wrapped around every sender's transport in SenderForKey,
// unconditionally: recordRawTrace redacts credentials before persisting, so
// capturing them here (to redact, not to store) is not itself a leak.
type headerCaptureTransport struct{ base http.RoundTripper }

func (t headerCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	h, _ := req.Context().Value(rawTraceHeaderKey{}).(*rawTraceHeaders)
	if h != nil {
		h.req = req.Header.Clone()
		h.reqPath = req.URL.Path
		// A SEPARATE reader: the outbound req.Body is never read, replaced or
		// mutated, so the capture cannot disturb the send it is observing.
		body, berr := readTraceBody(req)
		h.reqBody = body
		if berr != nil {
			h.captureErr = "request body capture: " + berr.Error()
		}
	}
	resp, err := base.RoundTrip(req)
	if h != nil && resp != nil {
		h.resp = resp.Header.Clone()
	}
	return resp, err
}

// readTraceBody replays the outbound request body through its own reader and
// decodes the Content-Encoding the transport is about to send. It never falls
// back to the compressed bytes: a body it cannot decode is reported, not
// stored in a form no reader can use.
func readTraceBody(req *http.Request) ([]byte, error) {
	if req.GetBody == nil {
		// Nothing replayable (no body at all, or a streaming one): nothing to
		// capture, and nothing went wrong.
		return nil, nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("reopen request body: %w", err)
	}
	r, err := decodeBody(rc, req.Header.Get("Content-Encoding"))
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	buf, rerr := io.ReadAll(io.LimitReader(r, maxTraceBody+1))
	cerr := r.Close()
	if rerr != nil {
		return nil, fmt.Errorf("read request body: %w", rerr)
	}
	if cerr != nil {
		return nil, fmt.Errorf("close request body: %w", cerr)
	}
	if len(buf) > maxTraceBody {
		return buf[:maxTraceBody], fmt.Errorf("decompressed body truncated at %d bytes", maxTraceBody)
	}
	return buf, nil
}

// bodyReader is a decompressing reader that closes the decoder and its source
// together, so no path leaks one without the other.
type bodyReader struct {
	io.Reader
	closers []io.Closer
}

// Close closes every layer, reporting the first failure.
func (b *bodyReader) Close() error {
	var first error
	for _, c := range b.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// decodeBody wraps src in the decompressors Content-Encoding names. The header
// lists encodings in the order they were APPLIED, so they are undone in
// reverse. An encoding this package does not implement is an error: silently
// passing compressed bytes through would record a body nothing can read.
func decodeBody(src io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	encodings := splitEncodings(contentEncoding)
	if len(encodings) == 0 {
		return src, nil
	}
	closers := []io.Closer{src}
	var r io.Reader = src
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encodings[i] {
		case "identity":
			continue
		case "gzip", "x-gzip":
			zr, err := gzip.NewReader(r)
			if err != nil {
				closeAll(closers)
				return nil, fmt.Errorf("gzip: %w", err)
			}
			r = zr
			closers = append(closers, zr)
		case "zstd":
			zr, err := zstd.NewReader(r)
			if err != nil {
				closeAll(closers)
				return nil, fmt.Errorf("zstd: %w", err)
			}
			rc := zr.IOReadCloser()
			r = rc
			closers = append(closers, rc)
		default:
			closeAll(closers)
			return nil, fmt.Errorf("unsupported content-encoding %q", encodings[i])
		}
	}
	return &bodyReader{Reader: r, closers: closers}, nil
}

// splitEncodings splits a Content-Encoding header into its lowercased tokens.
func splitEncodings(header string) []string {
	var out []string
	for _, part := range strings.Split(header, ",") {
		enc := strings.ToLower(strings.TrimSpace(part))
		if enc != "" {
			out = append(out, enc)
		}
	}
	return out
}

// closeAll closes every reader, discarding the errors: a caller that is already
// reporting a failure has nothing to add.
func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
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
