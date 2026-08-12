// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.graveland.dev/rafiki/pkg/routing"
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
	if got != 1 {
		t.Errorf("ejections counter = %v, want 1", got)
	}
}

// TestWatchProviderGuardNilSafe proves the watcher is a no-op with a nil
// guard, which is what RAFIKI_PROVIDER_GUARD=off leaves at the assembly site.
func TestWatchProviderGuardNilSafe(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.WatchProviderGuard(nil) // must not panic
}
