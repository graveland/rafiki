// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.graveland.dev/rafiki/pkg/routing"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/multigres/testkit/assert"
)

// TestWatchProviderGuardCountsEjections proves an ejection increments the
// counter, so a silent routing change is visible on the dashboard rather than
// only in the logs.
func TestWatchProviderGuardCountsEjections(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler))
	m.WatchProviderGuard(g)

	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{
			Provider: "CoreWeave", Model: "deepseek/deepseek-v4-pro", Conversation: "c1",
			PrefixHash: "h1", InputTokens: 50000, CacheReadTokens: 0,
		})
	}

	got := testutil.ToFloat64(m.ejections.WithLabelValues("coreweave", "deepseek/deepseek-v4-pro", "no_cache"))
	assert.NewCollecting(t).Eq(1, got, "ejections counter")
}

// TestWatchProviderGuardNilSafe proves the watcher is a no-op with a nil
// guard, which is what RAFIKI_PROVIDER_GUARD=off leaves at the assembly site.
func TestWatchProviderGuardNilSafe(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.WatchProviderGuard(nil) // must not panic
}

// gatherOpenMetrics renders reg in the OpenMetrics format exemplars need.
func gatherOpenMetrics(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}).ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestMetricsExemplar proves the latency histogram carries the active trace id
// as an exemplar inside a sampled span, and carries none outside one.
func TestMetricsExemplar(t *testing.T) {
	c := assert.NewCollecting(t)

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ctx, span := tp.Tracer("test").Start(context.Background(), "turn")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	m.ObserveTurn(ctx, "anthropic", "complete", "anthropic", time.Second, routing.CapturedUsage{InputTokens: 1})
	body := gatherOpenMetrics(t, reg)
	c.StrContains(body, `trace_id="`+traceID+`"`, "exemplar trace id in OpenMetrics output")

	// Outside any span: no exemplar at all.
	plainReg := prometheus.NewRegistry()
	plain := NewMetrics(plainReg)
	plain.ObserveTurn(context.Background(), "anthropic", "complete", "anthropic", time.Second, routing.CapturedUsage{InputTokens: 1})
	plainBody := gatherOpenMetrics(t, plainReg)
	c.False(strings.Contains(plainBody, "trace_id="), "no exemplar without a sampled span, got %s", plainBody)
}

// TestAgentMetrics proves the agent-side families increment and that an
// unpriced turn leaves rafiki_cost_usd_total absent for its labels.
func TestAgentMetrics(t *testing.T) {
	c := assert.NewCollecting(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveAgentTurn(context.Background(), "claude", "priced-model", "anthropic", "end_turn", 2*time.Second, 0.5)
	m.ObserveAgentTurn(context.Background(), "claude", "free-model", "anthropic", "end_turn", 2*time.Second, 0)
	m.ObserveTool(context.Background(), "claude", "Bash", false, 100*time.Millisecond)
	m.ObserveTool(context.Background(), "claude", "Bash", true, 200*time.Millisecond)

	c.Eq(1.0, testutil.ToFloat64(m.agentTurns.WithLabelValues("claude", "priced-model", "anthropic", "end_turn")), "turns counter")
	c.Eq(1.0, testutil.ToFloat64(m.toolCalls.WithLabelValues("claude", "Bash", "false")), "tool successes")
	c.Eq(1.0, testutil.ToFloat64(m.toolCalls.WithLabelValues("claude", "Bash", "true")), "tool errors")
	c.Eq(1, testutil.CollectAndCount(m.toolDur), "tool duration series")

	// Only the priced turn appears: costUSD == 0 must not create a zero series.
	want := `# HELP rafiki_cost_usd_total Agent spend in USD.
# TYPE rafiki_cost_usd_total counter
rafiki_cost_usd_total{kind="claude",model="priced-model",provider="anthropic"} 0.5
`
	c.Eq(nil, testutil.CollectAndCompare(m.costUSD, strings.NewReader(want)), "cost family")

	// A nil *Metrics is safe on every agent method.
	var nilMetrics *Metrics
	nilMetrics.ObserveAgentTurn(context.Background(), "claude", "m", "p", "end_turn", time.Second, 1)
	nilMetrics.ObserveTool(context.Background(), "claude", "Bash", true, time.Second)
	nilMetrics.ObserveTurn(context.Background(), "p", "error", "anthropic", time.Second, routing.CapturedUsage{})
}
