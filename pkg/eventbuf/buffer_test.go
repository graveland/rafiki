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
	mu     sync.Mutex
	calls  [][]string
	urgent []bool
}

func (r *flushRecorder) record(_, _ string, frags []string, urgent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(frags))
	copy(cp, frags)
	r.calls = append(r.calls, cp)
	r.urgent = append(r.urgent, urgent)
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
