package routing

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreaker(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b := NewBreaker(15 * time.Minute)

	// Closed initially → primary.
	if !b.UsePrimary(t0) {
		t.Fatal("fresh breaker must use primary")
	}
	// Retryable failure trips it Open.
	b.RecordResult(t0, true)
	// Before probeInterval elapses → fallback (not primary), no probe yet.
	if b.UsePrimary(t0.Add(5 * time.Minute)) {
		t.Error("before probeInterval elapses must route to fallback")
	}
	// After the probe interval → exactly one probe (primary), then fallback again until next window.
	probeAt := t0.Add(16 * time.Minute)
	if !b.UsePrimary(probeAt) {
		t.Error("after probe interval, one probe must use primary")
	}
	if b.UsePrimary(probeAt.Add(1 * time.Minute)) {
		t.Error("only one probe per interval; the next call must route to fallback")
	}
	// Probe fails → stays Open; next probe another interval later.
	b.RecordResult(probeAt, true)
	if b.UsePrimary(probeAt.Add(1 * time.Minute)) {
		t.Error("failed probe keeps it open within the new window")
	}
	// A probe that succeeds → Closed (primary from then on).
	nextProbe := probeAt.Add(16 * time.Minute)
	if !b.UsePrimary(nextProbe) {
		t.Fatal("probe expected")
	}
	b.RecordResult(nextProbe, false) // success
	if !b.UsePrimary(nextProbe.Add(1 * time.Minute)) {
		t.Error("after a healthy probe the breaker must be closed (primary)")
	}
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

	if got := probes.Load(); got != 1 {
		t.Fatalf("expected exactly one admitted probe among %d concurrent callers, got %d", n, got)
	}
}
