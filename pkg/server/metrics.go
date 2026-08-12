// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.graveland.dev/rafiki/pkg/routing"
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
	}
	reg.MustRegister(m.turns, m.latency, m.tokens, m.breakerState, m.breakerTrans, m.ejections, m.providersEjected)
	return m
}

// ObserveTurn records one resolved turn.
func (m *Metrics) ObserveTurn(upstream, status, protocol string, latency time.Duration, usage routing.CapturedUsage) {
	if m == nil {
		return
	}
	m.turns.WithLabelValues(upstream, status, protocol).Inc()
	m.latency.WithLabelValues(upstream, protocol).Observe(latency.Seconds())
	m.tokens.WithLabelValues(upstream, protocol, "input").Add(float64(usage.InputTokens))
	m.tokens.WithLabelValues(upstream, protocol, "output").Add(float64(usage.OutputTokens))
	m.tokens.WithLabelValues(upstream, protocol, "cache_read").Add(float64(usage.CacheReadTokens))
	m.tokens.WithLabelValues(upstream, protocol, "cache_creation").Add(float64(usage.CacheCreationTokens))
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
