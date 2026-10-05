// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routeExplainerEndpointsJSON is OpenRouter's /models/{id}/endpoints shape with
// two endpoints: one priced and tool-capable, and one whose prompt price is
// unparseable (a -1 → absent wire field) with a guessed slug.
const routeExplainerEndpointsJSON = `{"data":{"id":"deepseek/deepseek-v4.1-flash","endpoints":[
{"name":"DeepInfra","model_id":"deepseek/deepseek-v4.1-flash","provider_name":"DeepInfra","tag":"deepinfra/fp8","quantization":"fp8","pricing":{"prompt":"0.00000015","completion":"0.00000030"},"context_length":131072,"max_completion_tokens":16384,"supported_parameters":["tools"],"status":0,"uptime_last_30m":0.98},
{"name":"Example Provider","model_id":"deepseek/deepseek-v4.1-flash","provider_name":"Example Provider","tag":"","quantization":"unknown","pricing":{"prompt":"not-a-number","completion":"0.0000002"},"context_length":8192,"max_completion_tokens":null,"supported_parameters":[],"status":3,"uptime_last_30m":null}]}}`

// routeExplainerCatalog returns a catalog pointed at a fixture server serving
// routeExplainerEndpointsJSON.
func routeExplainerCatalog(t *testing.T) *routing.EndpointCatalog {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(routeExplainerEndpointsJSON))
	}))
	t.Cleanup(srv.Close)
	return routing.NewEndpointCatalogForTest(srv.Client(), srv.URL+"/%s", slog.New(slog.DiscardHandler))
}

func testGuard(t *testing.T) *routing.ProviderGuard {
	t.Helper()
	return routing.NewProviderGuard(0, slog.New(slog.DiscardHandler))
}

// fakeStats is a StatsSource that answers a fixed result or a fixed error.
type fakeStats struct {
	stats []routing.EndpointStats
	err   error
	got   string
}

func (f *fakeStats) Stats(_ context.Context, model string) ([]routing.EndpointStats, error) {
	f.got = model
	return f.stats, f.err
}

func routeRow(t *testing.T, resp *rafikiv1.ModelRoutesResponse, slug string) *rafikiv1.RouteEndpoint {
	t.Helper()
	for _, ep := range resp.Endpoints {
		if ep.Slug == slug {
			return ep
		}
	}
	t.Fatalf("no endpoint row for slug %q in %v", slug, resp.Endpoints)
	return nil
}

// TestConnectRoutesStatsFailureStillAnswers pins the advisory contract: a stats
// scrape that fails leaves a full endpoint list (prices, eligibility) and
// reports the failure in stats_note, rather than failing the RPC.
func TestConnectRoutesStatsFailureStillAnswers(t *testing.T) {
	c := assert.NewCollecting(t)
	stats := &fakeStats{err: errors.New("model page for x: status 503")}
	exp := connectRouteExplainer{cat: routeExplainerCatalog(t), stats: stats, guard: testGuard(t)}

	resp, err := exp.ExplainModelRoutes(context.Background(), "deepseek/deepseek-v4.1-flash")
	c.Require().NoError(err)
	c.Len(resp.Endpoints, 2, "endpoints still listed when stats fail")
	c.True(resp.StatsNote != "", "stats_note must carry the scrape failure, got %q", resp.StatsNote)
	c.Eq("deepseek/deepseek-v4.1-flash", stats.got, "the base model reaches the stats source")
	for _, ep := range resp.Endpoints {
		c.Nil(ep.P50TokensPerSec, "no stats attach when the scrape failed (slug %q)", ep.Slug)
	}
}

// TestConnectRoutesAppliesGuardIgnore pins that the explainer reads the SAME
// guard the request paths consult: an operator ban on a provider the fixture
// lists marks that row excluded with reason "banned".
func TestConnectRoutesAppliesGuardIgnore(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard(t)
	if _, err := g.Ban(context.Background(), time.Now(), "deepinfra", 0, ""); err != nil {
		t.Fatal(err)
	}
	exp := connectRouteExplainer{cat: routeExplainerCatalog(t), guard: g}

	// The openrouter/ prefix is stripped before the catalog fetch.
	resp, err := exp.ExplainModelRoutes(context.Background(), "openrouter/deepseek/deepseek-v4.1-flash")
	c.Require().NoError(err)
	banned := routeRow(t, resp, "deepinfra")
	c.Eq("banned", banned.ExcludedReason, "excluded_reason")
	c.False(banned.Eligible, "a banned provider is not eligible")
}

// TestConnectRoutesUnitConversion pins the units on the wire: prices arrive as
// USD per token and leave as USD per million tokens, a -1 price becomes an
// absent field (not zero), the spec round-trips canonically, and a matched
// stats row fills the throughput fields.
func TestConnectRoutesUnitConversion(t *testing.T) {
	c := assert.NewCollecting(t)
	stats := &fakeStats{stats: []routing.EndpointStats{{
		ProviderSlug: "deepinfra", Quantization: "fp8",
		P50Throughput: 42.5, P90Throughput: 51.25, P50LatencyMs: 120.5,
		Requests: 1234, WindowMinutes: 30,
	}}}
	exp := connectRouteExplainer{cat: routeExplainerCatalog(t), stats: stats, guard: testGuard(t)}

	resp, err := exp.ExplainModelRoutes(context.Background(),
		"deepseek/deepseek-v4.1-flash[sort=throughput,prefer=deepinfra]")
	c.Require().NoError(err)
	c.Eq("deepseek/deepseek-v4.1-flash", resp.Model, "base model, bracket stripped")
	c.Eq("sort=throughput,prefer=deepinfra", resp.Routing, "canonical spec string")

	deepinfra := routeRow(t, resp, "deepinfra")
	c.Require().NotNil(deepinfra.PromptUsdPerMtok, "prompt price present")
	c.InDelta(0.15, *deepinfra.PromptUsdPerMtok, 1e-9, "prompt USD per MTok")
	c.Require().NotNil(deepinfra.CompletionUsdPerMtok, "completion price present")
	c.InDelta(0.30, *deepinfra.CompletionUsdPerMtok, 1e-9, "completion USD per MTok")
	c.True(deepinfra.Preferred, "prefer= marks the slug preferred")

	c.Require().NotNil(deepinfra.P50TokensPerSec, "matched stats attach")
	c.InDelta(42.5, *deepinfra.P50TokensPerSec, 1e-9, "p50 throughput")
	c.Require().NotNil(deepinfra.P90TokensPerSec, "p90 throughput present")
	c.InDelta(51.25, *deepinfra.P90TokensPerSec, 1e-9, "p90 throughput")
	c.Require().NotNil(deepinfra.P50Latency, "p50 latency present")
	c.InDelta(120.5, deepinfra.P50Latency.AsDuration().Seconds()*1000, 1e-9, "p50 latency")
	c.Require().NotNil(deepinfra.StatsRequests, "request count present")
	c.Eq(int32(1234), *deepinfra.StatsRequests, "stats_requests")

	// An unparseable price is ABSENT, not zero — the whole reason the field is
	// optional on the wire.
	example := routeRow(t, resp, "example-provider")
	c.Nil(example.PromptUsdPerMtok, "an unknown price is absent, not zero")
	c.Require().NotNil(example.CompletionUsdPerMtok, "a known completion price is present")
	c.InDelta(0.2, *example.CompletionUsdPerMtok, 1e-9, "completion USD per MTok")
	c.Nil(example.P50TokensPerSec, "an unmatched endpoint carries no stats")
}
