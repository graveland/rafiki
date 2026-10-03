// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// endpointsJSON is OpenRouter's /api/v1/models/{id}/endpoints shape, with two
// endpoints: one fully populated, and one with a null uptime, a null
// max_completion_tokens and an empty tag (guessed slug).
const endpointsJSON = `{"data":{"id":"deepseek/deepseek-v4.1-flash","endpoints":[
{"name":"DeepInfra","model_id":"deepseek/deepseek-v4.1-flash","provider_name":"DeepInfra","tag":"deepinfra/fp8","quantization":"fp8","pricing":{"prompt":"0.00000014","completion":"0.00000028"},"context_length":131072,"max_completion_tokens":16384,"supported_parameters":["tools","temperature"],"status":0,"uptime_last_30m":0.98},
{"name":"Example Provider","model_id":"deepseek/deepseek-v4.1-flash","provider_name":"Example Provider","tag":"","quantization":"unknown","pricing":{"prompt":"not-a-number","completion":"0.0000002"},"context_length":8192,"max_completion_tokens":null,"supported_parameters":[],"status":3,"uptime_last_30m":null}]}}`

// endpointsServer serves body at status for every request and counts them.
func endpointsServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// testEndpointCatalog returns a catalog pointed at a server serving body, and
// the request counter.
func testEndpointCatalog(t *testing.T, status int, body string) (*EndpointCatalog, *atomic.Int32) {
	t.Helper()
	srv, hits := endpointsServer(t, status, body)
	return NewEndpointCatalogForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler)), hits
}

// TestEndpointCatalogParsesRows proves every field of an endpoint row is
// decoded: display name, tag/slug, quantization, both prices, context lengths,
// the nullable max-completion and uptime, tools support and status.
func TestEndpointCatalogParsesRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := testEndpointCatalog(t, http.StatusOK, endpointsJSON)
	eps, stale, err := c.Endpoints(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err, "Endpoints")
	ck.False(stale, "a fresh fetch must not be stale")
	ck.Len(eps, 2, "endpoints")

	first := eps[0]
	ck.Eq("DeepInfra", first.Provider, "Provider")
	ck.Eq("deepinfra", first.Slug, "Slug from tag")
	ck.Eq("deepinfra/fp8", first.Tag, "Tag")
	ck.Eq("fp8", first.Quantization, "Quantization")
	ck.InDelta(0.00000014, first.PromptPrice, 1e-12, "PromptPrice")
	ck.InDelta(0.00000028, first.CompletionPrice, 1e-12, "CompletionPrice")
	ck.Eq(131072, first.ContextLength, "ContextLength")
	ck.Eq(16384, first.MaxCompletion, "MaxCompletion")
	ck.NotNil(first.Uptime30m, "Uptime30m")
	if first.Uptime30m != nil {
		ck.InDelta(0.98, *first.Uptime30m, 1e-9, "Uptime30m")
	}
	ck.True(first.Tools, "Tools")
	ck.Eq(0, first.Status, "Status")

	second := eps[1]
	ck.Eq("Example Provider", second.Provider, "Provider")
	ck.Eq("example-provider", second.Slug, "an empty tag falls back to the guessed slug")
	ck.Eq("", second.Tag, "Tag")
	ck.Eq(-1.0, second.PromptPrice, "an unparseable price is -1")
	ck.InDelta(0.0000002, second.CompletionPrice, 1e-15, "CompletionPrice")
	ck.Eq(8192, second.ContextLength, "ContextLength")
	ck.Eq(0, second.MaxCompletion, "a null max_completion_tokens is 0")
	ck.Nil(second.Uptime30m, "a null uptime is nil")
	ck.False(second.Tools, "no \"tools\" in supported_parameters")
	ck.Eq(3, second.Status, "Status")
}

// TestEndpointCatalogUnparseablePriceIsMinusOne proves an unreadable price
// string becomes -1 and does not fail the row, and that a genuinely free
// endpoint's "0" stays 0 so free and unknown remain distinguishable.
func TestEndpointCatalogUnparseablePriceIsMinusOne(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":{"id":"m","endpoints":[
{"provider_name":"Free","tag":"free/fp16","pricing":{"prompt":"0","completion":"unknown-value"},"context_length":1,"supported_parameters":[],"status":0}]}}`
	c, _ := testEndpointCatalog(t, http.StatusOK, body)
	eps, _, err := c.Endpoints(context.Background(), "m")
	ck.NoError(err, "Endpoints")
	ck.Len(eps, 1, "endpoints")
	ck.Eq(0.0, eps[0].PromptPrice, "a free price is 0")
	ck.Eq(-1.0, eps[0].CompletionPrice, "an unparseable price is -1")
}

// TestEndpointCatalogCachesWithinTTL proves a cached entry answers without a
// second request until endpointCatalogTTL elapses.
func TestEndpointCatalogCachesWithinTTL(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, hits := testEndpointCatalog(t, http.StatusOK, endpointsJSON)
	now := time.Now()
	c.now = func() time.Time { return now }

	_, _, err := c.Endpoints(context.Background(), "m")
	ck.NoError(err, "first Endpoints")
	ck.Eq(int32(1), hits.Load(), "first call fetches")

	now = now.Add(endpointCatalogTTL - time.Minute)
	eps, stale, err := c.Endpoints(context.Background(), "m")
	ck.NoError(err, "second Endpoints")
	ck.False(stale, "still within TTL")
	ck.Len(eps, 2, "cached endpoints")
	ck.Eq(int32(1), hits.Load(), "a call within TTL must not fetch")
}

// TestEndpointCatalogServesStaleOnFetchFailure proves a failed refetch after
// the TTL keeps the last good entry: no error, stale true.
func TestEndpointCatalogServesStaleOnFetchFailure(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(endpointsJSON))
	}))
	t.Cleanup(srv.Close)
	c := NewEndpointCatalogForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler))
	now := time.Now()
	c.now = func() time.Time { return now }

	eps, stale, err := c.Endpoints(context.Background(), "m")
	ck.NoError(err, "first Endpoints")
	ck.False(stale, "first Endpoints")

	fail.Store(true)
	now = now.Add(endpointCatalogTTL + time.Minute)
	got, stale, err := c.Endpoints(context.Background(), "m")
	ck.NoError(err, "a failed refetch must serve the stale entry, not an error")
	ck.True(stale, "stale")
	ck.EqDeep(eps, got, "the stale entry is the last good one")
	ck.Eq(int32(2), hits.Load(), "the refetch was attempted")
}

// TestEndpointCatalogColdFailureIsAnError proves that with nothing cached and
// the fetch failing — a non-200, a malformed body, or a dead server — the
// error reaches the caller rather than an empty list.
func TestEndpointCatalogColdFailureIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"status", http.StatusInternalServerError, "boom"},
		{"malformed", http.StatusOK, "not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c, hits := testEndpointCatalog(t, tc.code, tc.body)
			eps, stale, err := c.Endpoints(context.Background(), "some/model")
			ck.Error(err, "a cold fetch failure must surface")
			ck.StrContains(err.Error(), "endpoints some/model:", "error names the model")
			ck.Nil(eps, "no endpoints on a cold failure")
			ck.False(stale, "nothing stale to serve")
			ck.Eq(int32(1), hits.Load(), "one attempt")
		})
	}
}

// TestEndpointCatalogSingleflightsConcurrentLoads proves concurrent callers
// for one uncached model share a single HTTP fetch.
func TestEndpointCatalogSingleflightsConcurrentLoads(t *testing.T) {
	ck := assert.NewAborting(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond) // hold the fetch open so callers overlap
		_, _ = w.Write([]byte(endpointsJSON))
	}))
	t.Cleanup(srv.Close)
	c := NewEndpointCatalogForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler))

	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := c.Endpoints(context.Background(), "m")
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		ck.NoError(err, "goroutine %d", i)
	}
	ck.Eq(int32(1), hits.Load(), "16 concurrent loads must make one request")
}

// TestEndpointCatalogRejectsEmptyList proves an empty endpoints array is an
// error rather than an empty success.
func TestEndpointCatalogRejectsEmptyList(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _ := testEndpointCatalog(t, http.StatusOK, `{"data":{"id":"m","endpoints":[]}}`)
	eps, stale, err := c.Endpoints(context.Background(), "m")
	ck.Error(err, "empty endpoints")
	ck.StrContains(err.Error(), "no endpoints listed", "error")
	ck.Nil(eps, "no endpoints")
	ck.False(stale, "not stale")
}
