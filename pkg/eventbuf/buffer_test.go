package eventbuf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/inbox"
)

// quietLogs silences the default logger for a test that deliberately provokes
// a warning, so a passing run prints nothing.
func quietLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// flushCall is one observed flush. The buffer no longer carries fragments, so
// what a flush means is "(childID, source) has rows waiting" — which is why
// the recorder snapshots the pending rows the owner would read.
type flushCall struct {
	childID string
	source  string
	orphans []inbox.Inbound
	pending []inbox.Inbound
}

type flushRecorder struct {
	mu    sync.Mutex
	st    *inbox.Memory
	calls []flushCall
}

func (r *flushRecorder) record(childID, source string, orphans []inbox.Inbound) {
	c := flushCall{childID: childID, source: source}
	c.orphans = append(c.orphans, orphans...)
	if r.st != nil {
		rows, err := r.st.Pending(context.Background(), childID)
		if err == nil {
			c.pending = rows
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *flushRecorder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *flushRecorder) call(i int) flushCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

func newTestBuffer(t *testing.T) (*Buffer, *FakeClock, *flushRecorder, *inbox.Memory) {
	t.Helper()
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	b := New(Config{
		Debounce:        5 * time.Second,
		MaxWait:         60 * time.Second,
		MaxBytesPerFrag: 8192,
	}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })
	return b, clk, rec, st
}

func pendingRows(t *testing.T, st *inbox.Memory, childID string) []inbox.Inbound {
	t.Helper()
	rows, err := st.Pending(context.Background(), childID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	return rows
}

// THE headline behaviour: five settles inside the window are one flush. What
// is IN that flush is the inbox's business now; that it happens once is this
// package's.
func TestFiveEventsInWindowProduceOneFlush(t *testing.T) {
	b, clk, rec, st := newTestBuffer(t)
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
	got := rec.call(0)
	if got.childID != "c_coord" || got.source != "subagents" {
		t.Fatalf("flush target = (%q, %q); want (c_coord, subagents)", got.childID, got.source)
	}
	if n := len(pendingRows(t, st, "c_coord")); n != 5 {
		t.Fatalf("persisted rows = %d; want 5 — the rows ARE the batch", n)
	}
}

// The durable write happens BEFORE the timer is armed. This is the exact
// window the lost-batch bug lived in: a daemon that died inside the quiet
// window used to lose the batch outright.
func TestPushPersistsBeforeArming(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	var flushes atomic.Int32
	b := New(Config{Debounce: time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(func(string, string, []inbox.Inbound) { flushes.Add(1) })
	b.SetBusy(func(string) bool { return false })

	b.Push("c_1", "subagents", "c_2", "agent c_2 settled")

	if flushes.Load() != 0 {
		t.Fatalf("flushes = %d; want 0 — the debounce has not elapsed", flushes.Load())
	}
	rows := pendingRows(t, st, "c_1")
	if len(rows) != 1 || rows[0].Source != "subagents" || rows[0].Key != "c_2" {
		t.Fatalf("rows = %+v, want one persisted fragment before the debounce elapsed", rows)
	}
	if rows[0].Mode != inbox.ModePrompt {
		t.Fatalf("mode = %v; want prompt", rows[0].Mode)
	}
}

// Max-wait: a steady drip faster than the debounce must still flush.
func TestMaxWaitCeilingPreventsStarvation(t *testing.T) {
	b, clk, rec, _ := newTestBuffer(t)
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
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{Debounce: 5 * time.Second, MaxWait: 60 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
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
	// The row waited in the store the whole time, not in the buffer.
	if n := len(rec.call(0).pending); n != 1 {
		t.Fatalf("pending rows at flush = %d; want 1", n)
	}
}

// PushNow skips the debounce, and the fragment already queued rides along.
// Ordering is a property of the ROWS now, not of an assembled slice the
// buffer hands over.
func TestPushNowBypassesDebounceAndDrainsPendingFirst(t *testing.T) {
	b, _, rec, _ := newTestBuffer(t)
	b.Push("c_w", "executor", "", "queued note")
	b.PushNow("c_w", "executor", "EXECUTOR LOST")
	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1 — PushNow must flush immediately, once", rec.n())
	}
	rows := rec.call(0).pending
	if len(rows) != 2 {
		t.Fatalf("pending rows at flush = %d (%+v); want 2", len(rows), rows)
	}
	if rows[0].Text != "queued note" || rows[1].Text != "EXECUTOR LOST" {
		t.Fatalf("row order = %q, %q; the queued fragment must precede the urgent one",
			rows[0].Text, rows[1].Text)
	}
}

// Per-fragment truncation still lives here: Buffer.accept truncates before the
// row is persisted, so the visible marker must be in the ROW. (The batch-level
// caps moved to inbox.Coalesce and are tested there.)
func TestPerFragmentTruncationIsVisibleInThePersistedRow(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	b := New(Config{Debounce: time.Second, MaxBytesPerFrag: 20}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(func(string, string, []inbox.Inbound) {})
	b.SetBusy(func(string) bool { return false })

	const long = "fragment number 0 with lots and lots of text"
	b.Push("c", "src", "", long)

	rows := pendingRows(t, st, "c")
	if len(rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(rows))
	}
	if len(rows[0].Text) > 20 {
		t.Fatalf("persisted text is %d bytes (%q); want <= MaxBytesPerFrag (20)",
			len(rows[0].Text), rows[0].Text)
	}
	if !strings.HasSuffix(rows[0].Text, "…(truncated)") {
		t.Fatalf("persisted text = %q; truncation must be VISIBLE — an event the "+
			"agent never sees and never learns it missed is the worst outcome available",
			rows[0].Text)
	}
}

func TestSourceWithDoubleColonIsRejected(t *testing.T) {
	quietLogs(t)
	b, clk, rec, st := newTestBuffer(t)
	b.Push("c", "bad::source", "", "x")
	clk.Advance(10 * time.Second)
	if rec.n() != 0 {
		t.Fatal("a source containing :: must be rejected, not routed")
	}
	if n := len(pendingRows(t, st, "c")); n != 0 {
		t.Fatalf("persisted rows = %d; a rejected source must not reach the store", n)
	}
}

// PushNow skips the DEBOUNCE. It does not skip the BUSY GATE — those are two
// independent bypasses and conflating them injects a steer into every urgent
// event, whether or not the turn it lands in is invalidated by it.
func TestPushNowDefersWhileBusy(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
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
	rows := pendingRows(t, st, "c_w")
	if len(rows) != 1 || rows[0].Text != "BUDGET EXHAUSTED" {
		t.Fatalf("rows = %+v; want the one urgent fragment", rows)
	}
	if rows[0].Mode != inbox.ModePrompt {
		t.Fatalf("mode = %v; PushNow must accept as a prompt, not a steer", rows[0].Mode)
	}
}

// PushSteer skips the IDLE GATE: an executor-loss event must reach a worker
// that would otherwise spend another 40s believing it still has one. The
// steer itself is DATA — the row's Mode — because inbox.Coalesce makes any
// group containing a steer deliver as a steer.
func TestPushSteerBypassesTheBusyGate(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	b := New(Config{}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return true })

	b.PushSteer("c_w", "executor", "EXECUTOR LOST")
	if rec.n() != 1 {
		t.Fatalf("PushSteer delivered %d batches; want 1 even though the child is busy", rec.n())
	}
	rows := pendingRows(t, st, "c_w")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v; want 1", rows)
	}
	if rows[0].Mode != inbox.ModeSteer {
		t.Fatalf("mode = %v; want steer — stickiness is carried by the row now", rows[0].Mode)
	}
}

// A batch still inside its debounce window must NOT ride out on someone
// else's idle transition — that turns every turn-end into an extra turn,
// which is the cost this package exists to remove.
func TestDrainIdleReleasesOnlyDeferredBatches(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
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

// flush must never run under b.mu: it is the owner's delivery path, a blocking
// write, and a producer that pushes from inside that path would deadlock on a
// re-entrant acquire.
func TestFlushRunsWithoutHoldingTheLock(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	b := New(Config{Debounce: time.Second}, clk)
	b.SetBusy(func(string) bool { return false })
	done := make(chan struct{}, 1)
	b.SetFlush(func(string, string, []inbox.Inbound) {
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

// Regression for a TOCTOU in fire's deferral branch: checking b.busy and
// marking the batch deferred must happen under ONE lock hold. If they were
// two separate critical sections (check, then re-lock to mark deferred), a
// concurrent DrainIdle could run in the gap, see nothing deferred yet, and
// complete having drained nothing — stranding the batch until the child's
// NEXT busy->idle cycle instead of the one already racing it. With no armed
// timer left, nothing else would ever release it.
//
// This test forces exactly that interleaving: busy() blocks while fire holds
// b.mu (proving the check happens lock-in-hand), and a concurrently-launched
// DrainIdle can therefore only ever observe the state either strictly before
// fire's critical section runs (nothing to do yet) or strictly after it
// completes (the batch freshly deferred) — never the torn state in between.
func TestFireBusyCheckAndDeferralAreAtomicWithDrainIdle(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	b := New(Config{}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
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

	waitFor := func(name string, ch chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not complete — possible deadlock", name)
		}
	}

	pushDone := make(chan struct{})
	go func() {
		b.PushNow("c_w", "budget", "BUDGET EXHAUSTED")
		close(pushDone)
	}()

	// Bounded: an unguarded receive here turns a regression that never
	// consults b.busy into a hang instead of a failure.
	waitFor("the busy check", busyCheckStarted) // fire now holds b.mu, if correct

	drainDone := make(chan struct{})
	go func() {
		// If the busy check were not covered by the same lock as the
		// deferral, this call could run in the gap and see nothing to
		// drain. Correctly, it can only block until fire finishes, then
		// observe the freshly-deferred batch.
		b.DrainIdle("c_w")
		close(drainDone)
	}()

	// Give the DrainIdle goroutine a chance to reach and block on b.mu
	// before letting the busy check (and the deferral that follows it)
	// proceed. Not required for correctness — Lock() blocks regardless of
	// scheduling order — but it exercises the intended contention path.
	time.Sleep(20 * time.Millisecond)
	close(releaseBusyCheck)

	waitFor("PushNow", pushDone)
	waitFor("DrainIdle", drainDone)

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1 — the batch must not be stranded by the race", rec.n())
	}
	rows := pendingRows(t, st, "c_w")
	if len(rows) != 1 || rows[0].Mode != inbox.ModePrompt {
		t.Fatalf("rows = %+v; want one prompt row", rows)
	}
}

// Forget discards SCHEDULING state only. The rows outlive the buffer: an
// exited child can be resumed, and its queue is exactly what a resume should
// run, so what happens to those rows is the controller's decision (Reset on
// exit, Drop when the child is forgotten for good) — not this package's.
func TestForgetStopsTimersButLeavesMessagesForTheController(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
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

	rows := pendingRows(t, st, "c_dead")
	if len(rows) != 1 || rows[0].Text != "worker finished" {
		t.Fatalf("rows after Forget = %+v; the message must SURVIVE — a resumable "+
			"child's queue is exactly what a resume should run", rows)
	}
}

// TestForgetFlushesOrphansRatherThanDroppingThem proves the doc comment's
// promise ("does NOT discard messages") actually holds for orphans, which
// have no durable row to fall back on -- Reset/Drop only ever apply to rows
// that made it into the store. Before this fix, Forget deleted the whole
// perKey, orphans included, with nothing to hand them to: a child dying right
// after a database blip lost exactly the fragments the orphan path exists to
// save, silently and without error.
func TestForgetFlushesOrphansRatherThanDroppingThem(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	st := failingStore{inbox.NewMemory()}
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return true }) // defer the timer-fired flush

	b.Push("c_dead", "subagents", "", "worker finished")
	clk.Advance(10 * time.Second) // timer fires, defers on busy -- orphan stays in pk.orphans

	if rec.n() != 0 {
		t.Fatalf("flush ran before Forget (n=%d); test setup did not defer as expected", rec.n())
	}

	b.Forget("c_dead")

	if rec.n() != 1 {
		t.Fatalf("flushes after Forget = %d; want 1 -- the orphan must be handed to the owner", rec.n())
	}
	got := rec.call(0)
	if got.childID != "c_dead" || got.source != "subagents" {
		t.Fatalf("flush call = %+v", got)
	}
	if len(got.orphans) != 1 || got.orphans[0].Text != "worker finished" {
		t.Fatalf("orphans = %+v; want the one undurable message Forget must not drop", got.orphans)
	}
}

// failingStore is a real store whose Accept always fails. Wrapping it in a
// real inbox.Queue reaches the orphan path through the same seam the daemon
// uses, rather than around it — and the embedded Memory still answers Pending,
// so a test can prove nothing was persisted.
type failingStore struct {
	*inbox.Memory
}

func (failingStore) Accept(context.Context, inbox.Inbound) (inbox.Inbound, error) {
	return inbox.Inbound{}, errors.New("store unavailable")
}

var _ inbox.Store = failingStore{}

// The orphan path is deliberate: when the durable accept fails the fragment is
// kept in memory and handed to flush, delivered without durability rather than
// dropped. Losing durability is bad; losing the notification is worse.
func TestAcceptFailureDeliversTheFragmentAsAnOrphan(t *testing.T) {
	quietLogs(t)
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	st := failingStore{inbox.NewMemory()}
	b := New(Config{Debounce: time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	b.Push("c_w", "subagents", "", "worker done")
	clk.Advance(time.Second)

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1 — a store failure must not swallow the notification", rec.n())
	}
	got := rec.call(0)
	if len(got.orphans) != 1 {
		t.Fatalf("orphans = %+v; want the one undurable message", got.orphans)
	}
	o := got.orphans[0]
	if o.Text != "worker done" || o.ChildID != "c_w" || o.Source != "subagents" {
		t.Fatalf("orphan = %+v; the whole Inbound must survive, not just its text", o)
	}
	if o.ID != "" {
		t.Fatalf("orphan carries ID %q; it was never stored, so the store assigned none", o.ID)
	}
}

// With no accepter at all — a unit test, or the pre-inbox daemon — every push
// is an orphan: still delivered, never persisted.
func TestNoAccepterMakesEveryPushAnOrphan(t *testing.T) {
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	b := New(Config{Debounce: time.Second}, clk)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	b.Push("c_w", "subagents", "", "a")
	b.Push("c_w", "subagents", "", "b")
	clk.Advance(time.Second)

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1", rec.n())
	}
	got := rec.call(0).orphans
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("orphans = %+v; want [a b] in push order", got)
	}
}

// The degraded path must not silently downgrade a steer. An orphan is the
// whole inbox.Inbound, so Mode travels with it: a PushSteer that could not be
// persisted still has to interrupt the turn it invalidates, which is the only
// reason it was a steer. Reducing the orphan to its text loses exactly this,
// and loses it during a database blip — when "executor lost" is most likely to
// be the news in flight.
func TestSteerSurvivesTheOrphanPath(t *testing.T) {
	quietLogs(t)
	clk := NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	st := failingStore{inbox.NewMemory()}
	b := New(Config{}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return true })

	b.PushSteer("c_w", "executor", "EXECUTOR LOST")

	if rec.n() != 1 {
		t.Fatalf("flushes = %d; want 1 — a store failure must not swallow a steer", rec.n())
	}
	got := rec.call(0).orphans
	if len(got) != 1 {
		t.Fatalf("orphans = %+v; want the one undurable message", got)
	}
	if got[0].Mode != inbox.ModeSteer {
		t.Fatalf("orphan mode = %v; want steer — a steer that arrives as a prompt "+
			"interrupts nothing, and the worker spends another turn believing it "+
			"still has an executor", got[0].Mode)
	}
	if got[0].Text != "EXECUTOR LOST" || got[0].Source != "executor" {
		t.Fatalf("orphan = %+v; the whole Inbound must survive", got[0])
	}
	// It really is the degraded path: nothing reached the store.
	if rows := pendingRows(t, st.Memory, "c_w"); len(rows) != 0 {
		t.Fatalf("persisted rows = %+v; want none — Accept failed", rows)
	}
}
