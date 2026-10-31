// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/responses"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/version"
)

// Endpoints. openai-responses has a canonical default (api.openai.com); the
// ChatGPT-subscription dialect has a FIXED one, which providers.Validate
// enforces by refusing a base_url on that kind.
const (
	openAIResponsesBaseURL = "https://api.openai.com/v1"
	chatGPTBaseURL         = "https://chatgpt.com/backend-api/codex"
)

// responsesMaxLineBytes caps one SSE line. It is not a formality: the endpoint
// sends whole output items on a single `data:` line, and a reasoning item's
// encrypted_content is a multi-kilobyte (sometimes multi-megabyte) base64 blob.
const responsesMaxLineBytes = 64 << 20

// defaultResponsesStreamIdleTimeout is how long the decoder waits for the next
// event before declaring the stream dead. codex uses 300 s; a reasoning model
// can sit silent for a long time before the first token, so a short timeout
// would fail healthy turns. Held in the sender (not hardcoded) so tests can
// shorten it.
const defaultResponsesStreamIdleTimeout = 300 * time.Second

// chatGPTInstructionsAsDeveloperMessage places the system prompt where current
// codex puts it: the first input item, as a developer message, with no top-level
// instructions field. Spike 2 got a 200 from the top-level form, which is what
// this sender uses by default, but codex no longer exercises that field, so a
// backend change is plausible; this constant makes it a one-line fix.
const chatGPTInstructionsAsDeveloperMessage = false

// responsesUserAgent is the User-Agent every Responses request carries. Every
// client in this space sends a real one: Go's default "Go-http-client/2.0" is
// exactly the kind of string a bot filter rejects, and a rejection here looks
// like an authentication failure.
func responsesUserAgent() string {
	return fmt.Sprintf("rafiki/%s (%s; %s)", version.String(), runtime.GOOS, runtime.GOARCH)
}

// TokenSource supplies the credential for one Responses request. Token is
// called per request; force=true asks for a refreshed credential after a 401.
//
// headers is where a credential contributes more than a bearer token: the
// ChatGPT backend also wants the account id that the login belongs to, and
// emitting it here keeps the daemon's credential store (which knows the
// account) the only thing that has to know.
type TokenSource interface {
	Token(ctx context.Context, force bool) (bearer string, headers http.Header, err error)
}

// StaticToken is the api_key_env TokenSource; an empty key sends no
// Authorization header.
//
// force is ignored: a static key cannot be refreshed, and answering with the
// same key is what makes the sender's "retry once after 401" a single retry
// rather than a loop.
type StaticToken string

func (t StaticToken) Token(context.Context, bool) (string, http.Header, error) {
	return string(t), nil, nil
}

// ModelFact is what the serving account's /models entry says about one model.
// It is the Responses codec's fact, aliased so callers keep their spelling.
type ModelFact = responses.ModelFact

// ModelFacts looks a model up in the owner's /models cache; ok=false when
// unknown. Nil is legal.
type ModelFacts = responses.ModelFacts

// UsageLimitError is a subscription usage limit: terminal for the turn, never
// retried, because the reset is hours or days away. The Responses codec builds
// it; no classifier in this package matches it.
type UsageLimitError = responses.UsageLimitError

// responsesSender is the Sender for providers.KindOpenAIResponses and
// providers.KindOpenAIResponsesChatGPT: the OpenAI Responses API, spoken
// through the Responses codec.
//
// The two modes differ in exactly four ways, and each is a field here rather
// than a second sender type: the base URL (fixed for ChatGPT), the credential
// source (an owner's login versus an env key), the request body's ChatGPT
// constraints (store false, stream always true, zstd, no temperature), and the
// per-owner cookie jar for the Cloudflare challenge the chatgpt.com edge issues.
//
// Provider.SessionHeader is deliberately not read: unlike the kinds that go
// through a RoundTripper, this sender rewrites the session id into the request
// BODY (prompt_cache_key), which is where the Responses API takes it, and sends
// the fixed session-id header only in ChatGPT mode.
type responsesSender struct {
	baseURL string // scheme+host+path prefix, trailing "/" trimmed; never empty
	chatGPT bool
	codec   responses.Codec

	tokens TokenSource // never nil: NewChatGPTResponsesSender refuses nil, newResponsesSender wraps the key

	client *http.Client

	// streamIdleTimeout is how long the stream decoder waits between events
	// before declaring the connection dead. A field (not the package constant)
	// so tests can shorten it.
	streamIdleTimeout time.Duration
	// maxLineBytes is the per-line ceiling handed to the stream decoder;
	// responsesMaxLineBytes unless a test lowers it.
	maxLineBytes int

	// turnState is x-codex-turn-state, the sticky-routing token the ChatGPT
	// backend issues and expects replayed within a turn. Codex scopes it to one
	// turn; this sender cannot see turn boundaries (a turn is a sequence of
	// Sends, and the same sender serves them all), so it keeps the latest value
	// per session, which spike 2 showed is accepted either way.
	mu        sync.Mutex
	turnState map[string]string

	// zstdEnc is created once per sender and reused: a fresh encoder per request
	// allocates its window per turn, and the window is the expensive part. The
	// mutex is what makes reuse safe (an encoder is not reentrant), and it is the
	// same mutex as the turn-state map's only for convenience.
	zstdEnc *zstd.Encoder
}

// newResponsesSender builds the KindOpenAIResponses sender, the api-key
// dialect. An empty BaseURL means api.openai.com.
func newResponsesSender(p providers.Provider, key string, rt http.RoundTripper) (*responsesSender, error) {
	base := p.BaseURL
	if base == "" {
		base = openAIResponsesBaseURL
	}
	return &responsesSender{
		baseURL:           strings.TrimRight(base, "/"),
		codec:             responses.Codec{Mode: responses.ModeAPI},
		tokens:            StaticToken(key),
		client:            &http.Client{Transport: headerCaptureTransport{base: rt}},
		streamIdleTimeout: defaultResponsesStreamIdleTimeout,
		maxLineBytes:      responsesMaxLineBytes,
	}, nil
}

// NewChatGPTResponsesSender builds the KindOpenAIResponsesChatGPT sender bound to
// one owner's TokenSource.
//
// Each sender owns its own cookie jar. The chatgpt.com edge answers a challenge
// with a Set-Cookie the follow-up request must replay; a jar shared between
// senders would let one owner's cookies ride another owner's requests, which is
// the same class of leak as sharing the token.
func NewChatGPTResponsesSender(p providers.Provider, ts TokenSource, facts ModelFacts, rt http.RoundTripper) (Sender, error) {
	if ts == nil {
		return nil, fmt.Errorf("llm: provider %q: kind %q needs a token source", p.Name, p.Kind)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("llm: provider %q: cookie jar: %w", p.Name, err)
	}
	return &responsesSender{
		baseURL: chatGPTBaseURL,
		chatGPT: true,
		codec: responses.Codec{
			Mode:       responses.ModeChatGPT,
			Facts:      facts,
			DevMessage: chatGPTInstructionsAsDeveloperMessage,
		},
		tokens:            ts,
		client:            &http.Client{Transport: headerCaptureTransport{base: rt}, Jar: jar},
		streamIdleTimeout: defaultResponsesStreamIdleTimeout,
		maxLineBytes:      responsesMaxLineBytes,
	}, nil
}

// Send issues one Responses call.
//
// In ChatGPT mode there is no non-streamed call to make (store must be false and
// the endpoint always streams), so Send opens the stream and accumulates it: the
// caller gets the same fully-formed response either way.
func (s *responsesSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	if s.chatGPT {
		return s.sendFromStream(ctx, req, opts)
	}
	opts.Stream = false
	_, resp, err := s.post(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	// responsesMaxLineBytes, not the error cap: a non-streamed reply carries whole
	// output items, and a reasoning item's encrypted content alone can be
	// megabytes.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesMaxLineBytes))
	resp.Body.Close()
	if err := s.errorFor(resp, body); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, fmt.Errorf("llm: openai-responses: read response: %w", readErr)
	}
	return s.codec.DecodeResponse(body)
}

// sendFromStream is ChatGPT mode's Send: stream, accumulate, and hand back the
// response. The stream's own error is returned verbatim so the classifiers above
// this package see the same values the streaming path produces.
func (s *responsesSender) sendFromStream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	dec, err := s.Stream(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := dec.Close(); cerr != nil {
			slog.Debug("llm: openai-responses: close stream failed", "error", cerr)
		}
	}()
	acc := msg.NewAccumulator()
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if aerr := acc.Add(ev); aerr != nil {
			return nil, fmt.Errorf("llm: openai-responses: accumulate stream event: %w", aerr)
		}
	}
	return acc.Response(), nil
}

// Stream opens one Responses call with the stream flag set and hands back the
// codec's decoder.
func (s *responsesSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	opts.Stream = true
	_, resp, err := s.post(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the body before closing: it is where a usage limit and the
		// classification live, and draining keeps the connection reusable.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		return nil, s.errorFor(resp, body)
	}
	return s.codec.NewStreamDecoder(resp.Body, s.streamIdleTimeout, s.maxLineBytes), nil
}

// post issues one request, retrying once after a 401 with a forced credential
// refresh. A second 401 is returned as-is: a credential the token source cannot
// fix is a configuration problem, and a third attempt would only spend more time
// reaching the same answer.
func (s *responsesSender) post(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*http.Request, *http.Response, error) {
	if opts.SessionID == "" {
		// The session id rides the body (prompt_cache_key) as well as the header,
		// so it is resolved before the body is encoded.
		opts.SessionID, _ = sessionIDFromContext(ctx)
	}
	sessionID := opts.SessionID
	body, err := s.codec.Encode(req, opts)
	if err != nil {
		return nil, nil, err
	}
	payload, err := s.encodeBody(body)
	if err != nil {
		return nil, nil, err
	}

	var httpReq *http.Request
	for attempt := 0; attempt < 2; attempt++ {
		bearer, extra, terr := s.tokens.Token(ctx, attempt > 0)
		if terr != nil {
			return nil, nil, fmt.Errorf("llm: openai-responses: resolve credential: %w", terr)
		}
		httpReq, err = s.newRequest(ctx, payload, opts.Stream, bearer, extra, sessionID)
		if err != nil {
			return nil, nil, err
		}
		resp, derr := s.client.Do(httpReq)
		if derr != nil {
			return nil, nil, fmt.Errorf("llm: openai-responses: post responses: %w", derr)
		}
		s.rememberTurnState(sessionID, resp.Header)
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return httpReq, resp, nil
		}
		// First 401: drain and close so the connection stays reusable, ask the
		// token source for a forced refresh, and try once more.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
	}
	return httpReq, nil, fmt.Errorf("llm: openai-responses: post responses: unauthorized")
}

// encodeBody compresses the request body when the mode needs it. The ChatGPT
// endpoint accepts plain JSON (spike 3) and zstd is an optimization, but it is
// the shape codex sends and it keeps a large tool schema from being the slowest
// part of a turn.
//
// Accept-Encoding is never set by hand anywhere in this sender: doing so turns
// off net/http's transparent gzip decompression, which would hand raw compressed
// bytes to the response parser.
func (s *responsesSender) encodeBody(body []byte) ([]byte, error) {
	if !s.chatGPT {
		return body, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.zstdEnc == nil {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			return nil, fmt.Errorf("llm: openai-responses: zstd encoder: %w", err)
		}
		s.zstdEnc = enc
	}
	return s.zstdEnc.EncodeAll(body, nil), nil
}

// newRequest builds one request with every header the mode requires.
func (s *responsesSender) newRequest(ctx context.Context, payload []byte, stream bool, bearer string, extra http.Header, sessionID string) (*http.Request, error) {
	url := s.baseURL + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: openai-responses: build request: %w", err)
	}
	// TokenSource headers first: the ones below are this package's contract and
	// a credential must not be able to redefine them.
	for name, values := range extra {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	// Keyless convention: an empty bearer sends no Authorization header at all.
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("originator", "rafiki")
	req.Header.Set("User-Agent", responsesUserAgent())
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if s.chatGPT {
		req.Header.Set("Content-Encoding", "zstd")
		if sessionID != "" {
			req.Header.Set("session-id", sessionID)
			if ts := s.turnStateFor(sessionID); ts != "" {
				req.Header.Set("x-codex-turn-state", ts)
			}
		}
	}
	return req, nil
}

// errorFor classifies a non-2xx response. A subscription usage limit becomes its
// own terminal type; everything else is an APIError carrying the status and the
// codec's classification, so a context overflow reaches trim's retry predicate
// and a 5xx stays retryable.
func (s *responsesSender) errorFor(resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if u := responses.UsageLimitFromResponse(resp.StatusCode, resp.Header, body); u != nil {
		return u
	}
	stored := body
	if len(stored) > maxErrorBody {
		stored = stored[:maxErrorBody]
	}
	return &msg.APIError{
		Status:     resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       stored,
		Kind:       responses.Classify(resp.StatusCode, resp.Header, body),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Dialect:    s.codec.Mode.Dialect(),
		Message:    responsesErrorMessage(body),
	}
}

// responsesErrorMessage reads an error message from the endpoint's body, in
// either the nested or the flat envelope.
func responsesErrorMessage(body []byte) string {
	var wire struct {
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ""
	}
	if wire.Error != nil && wire.Error.Message != "" {
		return wire.Error.Message
	}
	return wire.Message
}

// rememberTurnState records x-codex-turn-state for the session it arrived on.
func (s *responsesSender) rememberTurnState(sessionID string, h http.Header) {
	if sessionID == "" || h == nil {
		return
	}
	v := h.Get("x-codex-turn-state")
	if v == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnState == nil {
		s.turnState = make(map[string]string)
	}
	s.turnState[sessionID] = v
}

// turnStateFor reads the remembered turn state for a session.
func (s *responsesSender) turnStateFor(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnState[sessionID]
}
