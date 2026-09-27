// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestBreaker(t *testing.T) {
	c := assert.NewCollecting(t)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b := NewBreaker(15 * time.Minute)

	// Closed initially → primary.
	c.Require().True(b.UsePrimary(t0), "fresh breaker must use primary")
	// Retryable failure trips it Open.
	b.RecordResult(t0, true)
	// Before probeInterval elapses → fallback (not primary), no probe yet.
	c.False(b.UsePrimary(t0.Add(5*time.Minute)), "before probeInterval elapses must route to fallback")
	// After the probe interval → exactly one probe (primary), then fallback again until next window.
	probeAt := t0.Add(16 * time.Minute)
	c.True(b.UsePrimary(probeAt), "after probe interval, one probe must use primary")
	c.False(b.UsePrimary(probeAt.Add(1*time.Minute)), "only one probe per interval; the next call must route to fallback")
	// Probe fails → stays Open; next probe another interval later.
	b.RecordResult(probeAt, true)
	c.False(b.UsePrimary(probeAt.Add(1*time.Minute)), "failed probe keeps it open within the new window")
	// A probe that succeeds → Closed (primary from then on).
	nextProbe := probeAt.Add(16 * time.Minute)
	c.Require().True(b.UsePrimary(nextProbe), "probe expected")
	b.RecordResult(nextProbe, false) // success
	c.True(b.UsePrimary(nextProbe.Add(1*time.Minute)), "after a healthy probe the breaker must be closed (primary)")
}

// TestBreakerConcurrentProbeGuard verifies the mutex + single-probe-slot
// invariant under concurrency: once Open and past the probe interval, many
// simultaneous callers must yield exactly one primary probe, not one per
// goroutine. Run with -race.
func TestBreakerConcurrentProbeGuard(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b := NewBreaker(15 * time.Minute)

	// Trip it Open.
	b.RecordResult(t0, true)

	probeAt := t0.Add(16 * time.Minute) // past the probe interval

	const n = 50
	var wg sync.WaitGroup
	var probes atomic.Int32
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if b.UsePrimary(probeAt) {
				probes.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.NewAborting(t).Eq(1, probes.Load(), "expected exactly one admitted probe among %d concurrent callers, got", n)
}
