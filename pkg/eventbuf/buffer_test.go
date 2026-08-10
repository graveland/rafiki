package eventbuf

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBuffer(t *testing.T) (*Buffer, *FakeClock, *flushRecorder) {
	t.Helper()
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{
		Debounce:         5 * time.Second,
		MaxWait:          60 * time.Second,
		MaxFragments:     30,
		MaxBytesPerFrag:  8192,
		MaxBytesPerFlush: 65536,
	}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })
	return b, clk, rec
}

type flushRecorder struct {
	mu         sync.Mutex
	calls      [][]string
	deliveries []Delivery
}

func (r *flushRecorder) record(_, _ string, frags []string, d Delivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(frags))
	copy(cp, frags)
	r.calls = append(r.calls, cp)
	r.deliveries = append(r.deliveries, d)
}

func (r *flushRecorder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// THE headline behaviour: five settles inside the window are one frame.
func TestFiveEventsInWindowProduceOneFlush(t *testing.T) {
	b, clk, rec := newTestBuffer(t)
	for i := range 5 {
		b.Push("c_coord", "subagents", "", fmt.Sprintf("worker %d finished", i))
		clk.Advance(500 * time.Millisecond)
	}
	if rec.n() != 0 {
		t.Fatalf("flushed %d times before the debounce elapsed; want 0", rec.n())
	}
	clk.Advance(5 * time.Second)
	if rec.n() != 1 {
		t.Fatalf("flushed %d times; want exactly 1", rec.n())
	}
	if len(rec.calls[0]) != 5 {
		t.Fatalf("batch had %d fragments; want 5", len(rec.calls[0]))
	}
}

// Keyed coalescing is where the real compression lives: one worker
// transitioning five times is ONE fragment, not five.
func TestKeyedFragmentsCoalesceToLatest(t *testing.T) {
	b, clk, rec := newTestBuffer(t)
	for _, s := range []string{"streaming", "tool_error", "retry", "idle", "exited"} {
		b.Push("c_coord", "subagents", "c_worker", "c_worker: "+s)
	}
	clk.Advance(6 * time.Second)
	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1", rec.n())
	}
	if len(rec.calls[0]) != 1 {
		t.Fatalf("fragments = %d; want 1 — a keyed fragment is last-write-wins", len(rec.calls[0]))
	}
	if !strings.Contains(rec.calls[0][0], "exited") {
		t.Fatalf("kept %q; want the LATEST value", rec.calls[0][0])
	}
}

func TestDifferentKeysStaySeparate(t *testing.T) {
	b, clk, rec := newTestBuffer(t)
	b.Push("c_coord", "subagents", "c_a", "a: idle")
	b.Push("c_coord", "subagents", "c_b", "b: idle")
	clk.Advance(6 * time.Second)
	if len(rec.calls[0]) != 2 {
		t.Fatalf("fragments = %d; want 2 — different keys must not merge", len(rec.calls[0]))
	}
}

// Max-wait: a steady drip faster than the debounce must still flush.
func TestMaxWaitCeilingPreventsStarvation(t *testing.T) {
	b, clk, rec := newTestBuffer(t)
	// Push every 4s — always resets a 5s debounce — for 80s total.
	for range 20 {
		b.Push("c_coord", "subagents", "", "tick")
		clk.Advance(4 * time.Second)
	}
	if rec.n() == 0 {
		t.Fatal("a steady drip starved the flush; the max-wait ceiling is not enforced")
	}
}

// Deferral: a flush landing mid-turn must wait for the idle transition, NOT
// for a timeout. Asserting this specifically is what proves there is no
// hidden poller doing the work.
func TestFlushDefersWhileBusyAndReleasesOnIdle(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{
		Debounce:         5 * time.Second,
		MaxWait:          60 * time.Second,
		MaxFragments:     30,
		MaxBytesPerFrag:  8192,
		MaxBytesPerFlush: 65536,
	}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return busy.Load() })

	b.Push("c_coord", "subagents", "", "worker done")
	clk.Advance(10 * time.Second)
	if rec.n() != 0 {
		t.Fatal("flushed while the child was mid-turn")
	}
	// A long wait must NOT release it — only the idle transition does.
	clk.Advance(10 * time.Minute)
	if rec.n() != 0 {
		t.Fatal("a deferred batch escaped on a timer; it must wait for idle")
	}
	busy.Store(false)
	b.DrainIdle("c_coord")
	if rec.n() != 1 {
		t.Fatalf("flushes after idle = %d; want 1", rec.n())
	}
}

func TestPushNowBypassesDebounceAndDrainsPendingFirst(t *testing.T) {
	b, _, rec := newTestBuffer(t)
	b.Push("c_w", "executor", "", "queued note")
	b.PushNow("c_w", "executor", "EXECUTOR LOST")
	if rec.n() == 0 {
		t.Fatal("PushNow must flush immediately")
	}
	// Ordering: the queued fragment must appear before the urgent one,
	// whether in one batch or two consecutive ones.
	var all []string
	for _, c := range rec.calls {
		all = append(all, c...)
	}
	qi, ui := -1, -1
	for i, s := range all {
		if strings.Contains(s, "queued note") {
			qi = i
		}
		if strings.Contains(s, "EXECUTOR LOST") {
			ui = i
		}
	}
	if qi == -1 || ui == -1 || qi > ui {
		t.Fatalf("ordering lost: %v", all)
	}
}

func TestFragmentCapTruncatesVisibly(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{
		Debounce:         time.Second,
		MaxWait:          time.Minute,
		MaxFragments:     3,
		MaxBytesPerFrag:  20,
		MaxBytesPerFlush: 1000,
	}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	for i := range 10 {
		b.Push("c", "src", "", fmt.Sprintf("fragment number %d with lots of text", i))
	}
	clk.Advance(2 * time.Second)
	if len(rec.calls[0]) > 3 {
		t.Fatalf("fragments = %d; want <= 3", len(rec.calls[0]))
	}
	joined := strings.Join(rec.calls[0], "\n")
	if !strings.Contains(joined, "truncated") && !strings.Contains(joined, "omitted") {
		t.Fatal("truncation must be VISIBLE in the batch — an event the agent " +
			"never sees and never learns it missed is the worst outcome available")
	}
}

func TestSourceWithDoubleColonIsRejected(t *testing.T) {
	b, clk, rec := newTestBuffer(t)
	b.Push("c", "bad::source", "", "x")
	clk.Advance(10 * time.Second)
	if rec.n() != 0 {
		t.Fatal("a source containing :: must be rejected, not routed")
	}
}

// PushNow skips the DEBOUNCE. It does not skip the BUSY GATE — those are two
// independent bypasses and conflating them injects a steer into every urgent
// event, whether or not the turn it lands in is invalidated by it.
func TestPushNowDefersWhileBusy(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return busy.Load() })

	b.PushNow("c_w", "budget", "BUDGET EXHAUSTED")
	if rec.n() != 0 {
		t.Fatalf("PushNow delivered %d batches while the child was mid-turn; want 0", rec.n())
	}

	busy.Store(false)
	b.DrainIdle("c_w")
	if rec.n() != 1 {
		t.Fatalf("flushes after idle = %d; want 1", rec.n())
	}
	if rec.deliveries[0] != DeliverPrompt {
		t.Fatalf("delivery = %v; PushNow must use prompt, not steer", rec.deliveries[0])
	}
	if len(rec.calls[0]) != 1 || rec.calls[0][0] != "BUDGET EXHAUSTED" {
		t.Fatalf("batch = %v", rec.calls[0])
	}
}

// PushSteer skips the IDLE GATE: an executor-loss event must reach a worker
// that would otherwise spend another 40s believing it still has one.
func TestPushSteerBypassesTheBusyGate(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return true })

	b.PushSteer("c_w", "executor", "EXECUTOR LOST")
	if rec.n() != 1 {
		t.Fatalf("PushSteer delivered %d batches; want 1 even though the child is busy", rec.n())
	}
	if rec.deliveries[0] != DeliverSteer {
		t.Fatalf("delivery = %v; want DeliverSteer", rec.deliveries[0])
	}
}

// A batch still inside its debounce window must NOT ride out on someone
// else's idle transition — that turns every turn-end into an extra turn,
// which is the cost this package exists to remove.
func TestDrainIdleReleasesOnlyDeferredBatches(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	b.Push("c_w", "subagents", "", "worker 1 finished")
	b.DrainIdle("c_w")
	if rec.n() != 0 {
		t.Fatalf("DrainIdle flushed a batch still inside its debounce window")
	}

	clk.Advance(5 * time.Second)
	if rec.n() != 1 {
		t.Fatalf("the batch never flushed on its own timer; flushes = %d", rec.n())
	}
}

// flush must never run under b.mu: it is Controller.injectBatch → c.Send, a
// blocking write, and a producer that pushes from inside that path would
// deadlock on a re-entrant acquire.
func TestFlushRunsWithoutHoldingTheLock(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	b := New(Config{Debounce: time.Second}, clk)
	b.SetBusy(func(string) bool { return false })
	done := make(chan struct{}, 1)
	b.SetFlush(func(childID, source string, frags []string, d Delivery) {
		// Re-entering the buffer from inside flush must not deadlock.
		b.Push("c_other", "reentrant", "", "pushed from inside flush")
		done <- struct{}{}
	})

	b.Push("c_w", "subagents", "", "worker 1 finished")
	clk.Advance(time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flush deadlocked — it is being called while b.mu is held")
	}
}

// Regression for a TOCTOU in emit's redeposit branch: checking b.busy and
// marking the batch deferred must happen under ONE lock hold. If they were
// two separate critical sections (check, then re-lock to redeposit), a
// concurrent DrainIdle could run in the gap, see nothing deferred yet, and
// complete having drained nothing — stranding the batch until the child's
// NEXT busy->idle cycle instead of the one already racing it.
//
// This test forces exactly that interleaving: busy() blocks while emit
// holds b.mu (proving the check now happens lock-in-hand), and a
// concurrently-launched DrainIdle can therefore only ever observe the
// state either strictly before emit's critical section runs (nothing to
// do yet) or strictly after it completes (the batch freshly deferred) —
// never the torn state in between. Either way the batch must not be lost:
// the assertion below only holds because the second case is what actually
// happens once the busy check unblocks.
func TestEmitBusyCheckAndRedepositAreAtomicWithDrainIdle(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{}, clk)
	b.SetFlush(rec.record)

	busyCheckStarted := make(chan struct{})
	releaseBusyCheck := make(chan struct{})
	var busyCalls atomic.Int32
	b.SetBusy(func(string) bool {
		if busyCalls.Add(1) == 1 {
			close(busyCheckStarted)
			<-releaseBusyCheck
		}
		return true
	})

	pushDone := make(chan struct{})
	go func() {
		b.PushNow("c_w", "budget", "BUDGET EXHAUSTED")
		close(pushDone)
	}()

	<-busyCheckStarted // emit is now inside its busy check, holding b.mu if fixed

	drainDone := make(chan struct{})
	go func() {
		// If the busy check were not covered by the same lock as the
		// redeposit, this call could run in the gap and see nothing to
		// drain. With the fix it can only block until emit finishes, then
		// observe the freshly-redeposited, freshly-deferred batch.
		b.DrainIdle("c_w")
		close(drainDone)
	}()

	// Give the DrainIdle goroutine a chance to reach and block on b.mu
	// before letting the busy check (and the redeposit that follows it)
	// proceed. Not required for correctness — Lock() blocks regardless of
	// scheduling order — but it exercises the intended contention path.
	time.Sleep(20 * time.Millisecond)
	close(releaseBusyCheck)

	waitFor := func(name string, ch chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not complete — possible deadlock", name)
		}
	}
	waitFor("PushNow", pushDone)
	waitFor("DrainIdle", drainDone)

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1 — the batch must not be stranded by the race", rec.n())
	}
	if rec.deliveries[0] != DeliverPrompt {
		t.Fatalf("delivery = %v; want DeliverPrompt", rec.deliveries[0])
	}
}

// The byte cap must still apply AFTER the fragment cap has already trimmed
// the batch: an early return from the fragment-cap branch used to skip the
// byte budget entirely, which bites hardest exactly when the batch is
// largest. MaxFragments alone (30 at the default) never exercises this —
// the two caps must be pinned together to distinguish the fix from the bug.
func TestAssembleBatchAppliesByteCapAfterFragmentCap(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{
		Debounce:         time.Second,
		MaxWait:          time.Minute,
		MaxFragments:     10,
		MaxBytesPerFrag:  1000, // large enough that per-fragment truncation never triggers
		MaxBytesPerFlush: 25,
	}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	const fragment = "1234567890" // exactly 10 bytes
	for range 15 {
		b.Push("c", "src", "", fragment)
	}
	clk.Advance(2 * time.Second)

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1", rec.n())
	}
	got := rec.calls[0]

	// Fragment cap (10) keeps 9 real fragments out of 15 pushed (10-1, one
	// slot reserved for the marker) — 6 omitted there. The byte cap (25
	// bytes) then trims those 9 (90 bytes) down to 2 (20 bytes; a 3rd would
	// push to 30 > 25) — 7 more omitted. Under the original bug, the byte
	// cap never ran here at all: this batch would carry all 9 fragments
	// (90 bytes) instead of 2 (20 bytes).
	const wantKept = 2
	const wantOmitted = 6 + 7

	if len(got) != wantKept+1 { // +1 for the trailing omission marker
		t.Fatalf("fragments = %d (%v); want %d real + 1 marker", len(got), got, wantKept)
	}

	var totalBytes int
	for _, f := range got[:wantKept] {
		if f != fragment {
			t.Fatalf("fragment %q corrupted", f)
		}
		totalBytes += len(f)
	}
	if totalBytes > 25 {
		t.Fatalf("real fragment bytes = %d; must stay within MaxBytesPerFlush (25)", totalBytes)
	}

	marker := got[wantKept]
	wantMarker := fmt.Sprintf("[%d fragment(s) omitted]", wantOmitted)
	if marker != wantMarker {
		t.Fatalf("marker = %q; want %q", marker, wantMarker)
	}
}

func TestForgetDiscardsEverythingForADeadChild(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return true })

	b.Push("c_dead", "subagents", "", "worker finished")
	clk.Advance(10 * time.Second) // timer fires, defers on busy

	b.Forget("c_dead")

	b.SetBusy(func(string) bool { return false })
	b.DrainIdle("c_dead")
	if rec.n() != 0 {
		t.Fatalf("delivered %d batches to a forgotten child; want 0", rec.n())
	}
	clk.Advance(time.Hour)
	if rec.n() != 0 {
		t.Fatalf("a forgotten child's timer still fired")
	}
}
