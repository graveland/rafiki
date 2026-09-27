package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"
)

// An unrouted request must leave a trace. The face serves four paths and a real
// client asks for more; before this, anything else 404'd out of the ServeMux
// silently, which made "what does this client actually need?" unanswerable
// without reading its binary.
func TestUnroutedRequestsAreLoggedOnceEach(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var seen sync.Map
	h := unroutedHandler(logger, &seen)

	// The same path three times.
	for range 3 {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil))
		c.Require().Eq(http.StatusNotFound, rec.Code, "unrouted path returned")
	}
	// A different one.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/hello", nil))
	c.Require().Eq(http.StatusNotFound, rec.Code, "unrouted path returned")

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	// The dedup key is method+path; count the logged `path=` attribute rather than
	// the bare substring, which now also appears in the "serves" list.
	c.Eq(1, strings.Count(out, "path=/v1/messages/count_tokens"), "count_tokens logged")
	c.StrContains(out, "/api/hello", "a second distinct path was not logged:\n")
	// The point of the line is to be actionable.
	c.StrContains(out, "/v1/messages", "the warning should name what IS served:\n")
}

// syncWriter guards the buffer: slog writes from the request goroutine and the
// test reads from its own.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
