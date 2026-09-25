// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRateLimitObserver records what the proxy told it, so the 429 and
// clean-completion call sites can be asserted without a daemon behind them.
type stubRateLimitObserver struct {
	mu      sync.Mutex
	limited map[string]time.Time
	oks     []string
}

func newStubRateLimitObserver() *stubRateLimitObserver {
	return &stubRateLimitObserver{limited: make(map[string]time.Time)}
}

func (s *stubRateLimitObserver) RateLimited(session string, resetAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limited[session] = resetAt
}

func (s *stubRateLimitObserver) TurnSucceeded(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oks = append(s.oks, session)
}

func newRateLimitTestProxy(t *testing.T, upstreamURL string, fs *fakeProxyStore) (*MessagesProxy, *stubRateLimitObserver) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstreamURL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	obs := newStubRateLimitObserver()
	p.SetRateLimitObserver(obs)
	return p, obs
}

// A 429 from a genuine passthrough response carries the unified rate-limit
// headers; the observer must see the child's session and the reset of the
// window whose status says it is doing the limiting.
func TestRateLimitObserverSeesA429WithTheLimitingWindowsReset(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "blocked")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "blocked")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", reset5h.Format(time.RFC3339))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", reset7d.Format(time.RFC3339))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
	}))
	defer upstream.Close()

	p, obs := newRateLimitTestProxy(t, upstream.URL, &fakeProxyStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "c_child1")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passed through", rec.Code)
	}
	got, ok := obs.limited["c_child1"]
	if !ok {
		t.Fatal("observer was not told about the 429")
	}
	if !got.Equal(reset5h) {
		t.Errorf("resetAt = %v, want the 5h (limiting) window's reset %v", got, reset5h)
	}
	if len(obs.oks) != 0 {
		t.Errorf("TurnSucceeded = %v, want none", obs.oks)
	}
}

// When no window's status identifies the limiter, the earliest reset of any
// window is the safe choice: firing early just gets another 429, which
// re-arms. A Retry-After is only consulted when no unified header parsed.
func TestRateLimitResetAtPrefersEarliestWhenNoWindowIsIdentified(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", reset5h.Format(time.RFC3339))
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", reset7d.Format(time.RFC3339))
	h.Set("Retry-After", "9999")
	if got := rateLimitResetAt(h); !got.Equal(reset5h) {
		t.Errorf("rateLimitResetAt = %v, want the earliest window reset %v (Retry-After must not win)", got, reset5h)
	}
}

// A 429 with neither a unified header nor Retry-After reports the zero time:
// the scheduler's own backoff ladder applies, and zero must never read as
// "already reset".
func TestRateLimitResetAtIsZeroWhenNothingNamesAReset(t *testing.T) {
	if got := rateLimitResetAt(http.Header{}); !got.IsZero() {
		t.Errorf("rateLimitResetAt = %v, want the zero time", got)
	}
	h := http.Header{}
	h.Set("Retry-After", "nonsense")
	if got := rateLimitResetAt(h); !got.IsZero() {
		t.Errorf("unparseable Retry-After = %v, want the zero time", got)
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	before := time.Now()
	if got := parseRetryAfterHeader("120"); got.Before(before.Add(119*time.Second)) || got.After(before.Add(121*time.Second)) {
		t.Errorf("delay-seconds form = %v, want ~now+120s", got)
	}
	date := time.Now().Add(3 * time.Minute).UTC().Format(http.TimeFormat)
	want, _ := http.ParseTime(date)
	if got := parseRetryAfterHeader(date); !got.Equal(want) {
		t.Errorf("HTTP-date form = %v, want %v", got, want)
	}
	if got := parseRetryAfterHeader(""); !got.IsZero() {
		t.Errorf("empty = %v, want zero", got)
	}
}

// A clean completion is the verdict that clears the watch; the observer must
// see it with the child's session.
func TestRateLimitObserverSeesCleanCompletions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	p, obs := newRateLimitTestProxy(t, upstream.URL, &fakeProxyStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "c_child1")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(obs.oks) != 1 || obs.oks[0] != "c_child1" {
		t.Errorf("TurnSucceeded = %v, want exactly one call for c_child1", obs.oks)
	}
	if len(obs.limited) != 0 {
		t.Errorf("RateLimited = %v, want none", obs.limited)
	}
}

// A non-429 failure is not the watch's business: the auto-resume exists for
// rate limits specifically, and a 5xx turn failure must not schedule one.
func TestRateLimitObserverIgnoresNon429Failures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error"}}`)
	}))
	defer upstream.Close()

	p, obs := newRateLimitTestProxy(t, upstream.URL, &fakeProxyStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "c_child1")
	p.ServeHTTP(rec, req)

	if len(obs.limited) != 0 || len(obs.oks) != 0 {
		t.Errorf("observer was told limited=%v oks=%v, want silence on a 5xx", obs.limited, obs.oks)
	}
}

// The gating predicate: capture off, a Task subagent's thread, or a missing
// session header must all silence the observer — a subagent's 429 must never
// schedule a resume for its parent, and a hand-configured client's session
// resolves to no child at all.
func TestRateLimitWatchableGates(t *testing.T) {
	if got := rateLimitWatchable(captureRef{}); got != "" {
		t.Errorf("capture-off ref = %q, want \"\"", got)
	}
	if got := rateLimitWatchable(captureRef{on: true, session: "c_child1", isSubagent: true}); got != "" {
		t.Errorf("subagent ref = %q, want \"\" (a subagent's 429 must not schedule its parent's resume)", got)
	}
	if got := rateLimitWatchable(captureRef{on: true, session: ""}); got != "" {
		t.Errorf("session-less ref = %q, want \"\" (hand-configured client)", got)
	}
	if got := rateLimitWatchable(captureRef{on: true, session: "c_child1"}); got != "c_child1" {
		t.Errorf("main-thread ref = %q, want c_child1", got)
	}
}
