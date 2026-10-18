// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.opentelemetry.io/otel/trace"
)

// Metrics is the server's Prometheus instrumentation: turn outcomes by
// upstream/status/protocol, latency, token counters (cache_read ratio is
// derivable), and per-upstream breaker state + transitions.
type Metrics struct {
	turns            *prometheus.CounterVec
	latency          *prometheus.HistogramVec
	tokens           *prometheus.CounterVec
	breakerState     *prometheus.GaugeVec
	breakerTrans     *prometheus.CounterVec
	ejections        *prometheus.CounterVec
	providersEjected *prometheus.GaugeVec

	agentTurns   *prometheus.CounterVec
	agentTurnDur *prometheus.HistogramVec
	toolCalls    *prometheus.CounterVec
	toolDur      *prometheus.HistogramVec
	costUSD      *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_llm_turns_total",
			Help: "LLM turns by upstream, terminal status and wire protocol.",
		}, []string{"upstream", "status", "protocol"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rafiki_llm_turn_latency_seconds",
			Help:    "LLM turn latency (full stream duration).",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"upstream", "protocol"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_llm_tokens_total",
			Help: "Token usage by kind (input, output, cache_read, cache_creation).",
		}, []string{"upstream", "protocol", "kind"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rafiki_breaker_open",
			Help: "1 when the upstream's breaker is open (pinned to fallback).",
		}, []string{"upstream"}),
		breakerTrans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_breaker_transitions_total",
			Help: "Breaker state transitions (trip / close).",
		}, []string{"upstream", "to"}),
		ejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_provider_ejections_total",
			Help: "Providers ejected from OpenRouter routing, by reason.",
		}, []string{"provider", "model_line", "reason"}),
		providersEjected: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rafiki_providers_ejected",
			Help: "Providers currently ejected for a model line.",
		}, []string{"model_line"}),
		agentTurns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_turns_total",
			Help: "Agent turns by child kind, model, provider and stop reason.",
		}, []string{"kind", "model", "provider", "stop_reason"}),
		agentTurnDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rafiki_turn_duration_seconds",
			Help:    "Agent turn wall time.",
			Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 1800},
		}, []string{"kind", "model", "provider"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_tool_calls_total",
			Help: "Tool calls by child kind, tool and outcome.",
		}, []string{"kind", "tool", "error"}),
		toolDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rafiki_tool_duration_seconds",
			Help:    "Tool call wall time.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300},
		}, []string{"kind", "tool"}),
		costUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_cost_usd_total",
			Help: "Agent spend in USD.",
		}, []string{"kind", "model", "provider"}),
	}
	reg.MustRegister(m.turns, m.latency, m.tokens, m.breakerState, m.breakerTrans, m.ejections, m.providersEjected,
		m.agentTurns, m.agentTurnDur, m.toolCalls, m.toolDur, m.costUSD)
	return m
}

// observeWithExemplar observes v on o, attaching the active trace id as an
// exemplar when the ctx carries a sampled span (OpenMetrics exposition).
func observeWithExemplar(ctx context.Context, o prometheus.Observer, v float64) {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && sc.IsSampled() {
		if eo, ok := o.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(v, prometheus.Labels{"trace_id": sc.TraceID().String()})
			return
		}
	}
	o.Observe(v)
}

// ObserveTurn records one resolved turn.
func (m *Metrics) ObserveTurn(ctx context.Context, upstream, status, protocol string, latency time.Duration, usage routing.CapturedUsage) {
	if m == nil {
		return
	}
	m.turns.WithLabelValues(upstream, status, protocol).Inc()
	observeWithExemplar(ctx, m.latency.WithLabelValues(upstream, protocol), latency.Seconds())
	m.tokens.WithLabelValues(upstream, protocol, "input").Add(float64(usage.InputTokens))
	m.tokens.WithLabelValues(upstream, protocol, "output").Add(float64(usage.OutputTokens))
	m.tokens.WithLabelValues(upstream, protocol, "cache_read").Add(float64(usage.CacheReadTokens))
	m.tokens.WithLabelValues(upstream, protocol, "cache_creation").Add(float64(usage.CacheCreationTokens))
}

// ObserveAgentTurn records one agent turn: its outcome by kind/model/provider/
// stop reason, its wall time, and — when priced — its spend.
func (m *Metrics) ObserveAgentTurn(ctx context.Context, kind, model, provider, stopReason string, d time.Duration, costUSD float64) {
	if m == nil {
		return
	}
	m.agentTurns.WithLabelValues(kind, model, provider, stopReason).Inc()
	observeWithExemplar(ctx, m.agentTurnDur.WithLabelValues(kind, model, provider), d.Seconds())
	if costUSD > 0 {
		m.costUSD.WithLabelValues(kind, model, provider).Add(costUSD)
	}
}

// ObserveTool records one tool call: its outcome and its wall time.
func (m *Metrics) ObserveTool(ctx context.Context, kind, tool string, isError bool, d time.Duration) {
	if m == nil {
		return
	}
	errLabel := "false"
	if isError {
		errLabel = "true"
	}
	m.toolCalls.WithLabelValues(kind, tool, errLabel).Inc()
	observeWithExemplar(ctx, m.toolDur.WithLabelValues(kind, tool), d.Seconds())
}

// WatchBreaker wires the state gauge + transition counter (and is safe to
// call with nil breaker).
func (m *Metrics) WatchBreaker(upstream string, b *routing.Breaker) {
	if m == nil || b == nil {
		return
	}
	m.breakerState.WithLabelValues(upstream).Set(0)
	b.SetOnTransition(func(open bool, _ time.Time) {
		to := "closed"
		v := 0.0
		if open {
			to, v = "open", 1
		}
		m.breakerState.WithLabelValues(upstream).Set(v)
		m.breakerTrans.WithLabelValues(upstream, to).Inc()
	})
}

// WatchProviderGuard wires the ejection counter and the currently-ejected
// gauge. Safe with a nil guard.
func (m *Metrics) WatchProviderGuard(g *routing.ProviderGuard) {
	if m == nil || g == nil {
		return
	}
	g.SetOnEject(func(provider, modelLine string, reason routing.EjectReason) {
		m.ejections.WithLabelValues(strings.ToLower(provider), modelLine, string(reason)).Inc()
		counts := map[string]int{}
		for _, e := range g.Ejected(time.Now()) {
			counts[e.ModelLine]++
		}
		for line, n := range counts {
			m.providersEjected.WithLabelValues(line).Set(float64(n))
		}
	})
}
