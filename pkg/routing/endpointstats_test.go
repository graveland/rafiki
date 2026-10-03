// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// modelPageFixture is the trimmed live OpenRouter model page (still escaped,
// with its "# trimmed from …" header line, exactly as served).
func modelPageFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/modelpage_deepseek_v4_1_flash.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// pageServer serves body on every request and counts hits; the body pointer can
// be swapped mid-test to simulate a later failing scrape.
func pageServer(t *testing.T, body *string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(*body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testPageStats(t *testing.T, body string) (*PageStats, *atomic.Int32) {
	t.Helper()
	srv, hits := pageServer(t, &body)
	return NewPageStatsForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler)), hits
}

// TestPageStatsParsesFixture proves the scraper reads provider slug,
// quantization and the reported percentiles out of a real (trimmed) model page,
// deduping endpoints that appear twice and keeping distinct endpoints that share
// a provider slug.
func TestPageStatsParsesFixture(t *testing.T) {
	ck := assert.NewCollecting(t)
	p, _ := testPageStats(t, modelPageFixture(t))

	rows, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err, "Stats of the fixture")
	ck.Len(rows, 7, "unique endpoints in the fixture")

	// Two endpoints share provider_slug "baseten/fp8" with distinct endpoint
	// ids; deduping by slug (rather than endpoint id) would collapse them to 1.
	baseten := 0
	for _, r := range rows {
		if r.ProviderSlug == "baseten/fp8" {
			baseten++
		}
	}
	ck.Eq(2, baseten, "distinct endpoints sharing provider_slug baseten/fp8")

	together, ok := findStats(rows, "together")
	ck.True(ok, "together has a row")
	ck.Eq("unknown", together.Quantization, "together quantization")
	ck.Eq(228.0, together.P50Throughput, "together p50 throughput")
	ck.Eq(355.0, together.P90Throughput, "together p90 throughput")
	ck.Eq(276.0, together.P50LatencyMs, "together p50 latency")
	ck.Eq(225332, together.Requests, "together request count")
	ck.Eq(30, together.WindowMinutes, "together window")
}

// TestPageStatsDedupesRepeatedEndpoints proves an endpoint appearing twice in
// the payload yields one row.
func TestPageStatsDedupesRepeatedEndpoints(t *testing.T) {
	ck := assert.NewCollecting(t)
	p, _ := testPageStats(t, modelPageFixture(t))

	rows, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err)
	ck.Len(rows, 7, "the duplicate occurrence collapses to 7 unique endpoints")
}

// TestPageStatsSkipsMalformedEndpoint proves one broken stats object drops only
// its own endpoint; the rest of the page still parses.
func TestPageStatsSkipsMalformedEndpoint(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := modelPageFixture(t)
	// Corrupt together's stats object (it appears once) with a key left without
	// a value, so its JSON decode fails but the marker still matches.
	broken := strings.Replace(body,
		`\"stats\":{\"endpoint_id\":\"57e1cb3e-9762-4ee1-a67c-259bd3af24b7\"`,
		`\"stats\":{\"endpoint_id\":\"57e1cb3e-9762-4ee1-a67c-259bd3af24b7\",\"p50_throughput\":`,
		1)
	ck.NotEq(body, broken, "fixture was modified")
	p, _ := testPageStats(t, broken)

	rows, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err, "a malformed endpoint is skipped, not fatal")
	ck.Len(rows, 6, "one fewer row than the intact fixture")
	if _, ok := findStats(rows, "together"); ok {
		ck.Fail("the malformed endpoint must be skipped")
	}
}

// TestPageStatsNoStatsIsAnErrorNotAPanic proves a 200 body with no endpoint
// stats returns the page-format error rather than panicking.
func TestPageStatsNoStatsIsAnErrorNotAPanic(t *testing.T) {
	ck := assert.NewCollecting(t)
	p, _ := testPageStats(t, `<html></html>`)

	var rows []EndpointStats
	var err error
	ck.NotPanics(func() { rows, err = p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash") }, "Stats must not panic")
	ck.Empty(rows, "no rows on a statless page")
	ck.ErrorContains(err, "page format may have changed")
}

// TestPageStatsServesStaleOnFailure proves a failed re-scrape keeps serving the
// last successful scrape instead of returning an error.
func TestPageStatsServesStaleOnFailure(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := modelPageFixture(t)
	srv, hits := pageServer(t, &body)
	p := NewPageStatsForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler))

	first, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err)
	ck.Len(first, 7)

	// Force the cached scrape stale, then make the next fetch fail.
	p.mu.Lock()
	p.entries["deepseek/deepseek-v4.1-flash"].fetched = time.Now().Add(-2 * pageStatsTTL)
	p.mu.Unlock()
	body = "<html>500</html>"

	second, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.NoError(err, "a failed refresh serves the stale scrape")
	ck.EqDeep(first, second, "the stale rows are returned")
	ck.Greater(int32(1), hits.Load(), "the stale path still hit the server")
}

// TestPageStatsOversizeBodyIsAnError proves a response past the read cap is an
// error rather than a parse of truncated bytes.
func TestPageStatsOversizeBodyIsAnError(t *testing.T) {
	ck := assert.NewCollecting(t)
	p, _ := testPageStats(t, strings.Repeat("a", pageStatsMaxBytes+1))

	_, err := p.Stats(context.Background(), "deepseek/deepseek-v4.1-flash")
	ck.ErrorContains(err, "exceeds")
}

// TestPageStatsNullPercentileIsAbsent proves a null percentile is preserved as
// the absent sentinel -1 rather than collapsing into a reported zero.
func TestPageStatsNullPercentileIsAbsent(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"id":"11111111-1111-1111-1111-111111111111","provider_slug":"acme/fp8",` +
		`"quantization":"fp8","stats":{"endpoint_id":"11111111-1111-1111-1111-111111111111",` +
		`"p50_latency":null,"p50_throughput":null,"p90_throughput":40,"request_count":null,` +
		`"window_minutes":30}}`
	p, _ := testPageStats(t, body)

	rows, err := p.Stats(context.Background(), "acme/model")
	ck.NoError(err)
	ck.Len(rows, 1)
	ck.Eq(-1.0, rows[0].P50Throughput, "a null percentile is absent")
	ck.Eq(-1.0, rows[0].P50LatencyMs, "a null latency is absent")
	ck.Eq(40.0, rows[0].P90Throughput, "a reported percentile survives")
	ck.Eq(-1, rows[0].Requests, "a null count is absent")
	ck.Eq(30, rows[0].WindowMinutes)
}

func findStats(rows []EndpointStats, slug string) (EndpointStats, bool) {
	for _, r := range rows {
		if r.ProviderSlug == slug {
			return r, true
		}
	}
	return EndpointStats{}, false
}
