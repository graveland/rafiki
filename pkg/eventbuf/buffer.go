package eventbuf

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/inbox"
)

// Config controls event buffer behaviour. Zero values are replaced with
// sensible defaults by New.
//
// The batch-shaping knobs (fragment count, total bytes) live in
// inbox.BatchConfig now: coalescing happens at delivery, over the rows, so the
// buffer has no batch to shape. What is left here is per-fragment truncation,
// which happens on the way IN — an oversized fragment must never reach the
// store in the first place.
type Config struct {
	Debounce        time.Duration // quiet window after last push before flush (default 5s)
	MaxWait         time.Duration // ceiling measured from first push (default 60s)
	MaxBytesPerFrag int           // max bytes per fragment before truncation (default 8192)
}

// FlushFn asks the owner to deliver whatever is queued for (childID, source).
//
// It carries no fragments: the rows ARE the batch, and the owner reads them
// from the inbox. orphans is the degraded path only — messages whose durable
// accept failed, which the owner injects directly so a database blip costs
// durability rather than the notification itself.
//
// An orphan is the whole inbox.Inbound that could not be stored, not just its
// text, so everything that shapes delivery travels with it — Mode above all.
// Reducing it to a string silently downgrades a PushSteer to a prompt exactly
// when the news is urgent enough to have been a steer. The owner is expected to
// run orphans through inbox.Coalesce like any other rows, so the degraded path
// gets the same grouping, caps and any-steer-makes-it-a-steer rule as the
// durable one rather than a second implementation that drifts.
type FlushFn func(childID, source string, orphans []inbox.Inbound)

// BusyFn reports whether childID is mid-turn. A flush is deferred while true.
type BusyFn func(childID string) bool

type bufKey struct {
	childID string
	source  string
}

type perKey struct {
	firstPushAt time.Time
	timer       Timer
	dirty       bool            // something was accepted since the last flush
	deferred    bool            // a flush was withheld because the child was busy
	orphans     []inbox.Inbound // messages the store refused; delivered without durability
}

// Buffer schedules the delivery of externally-injected agent events. It keeps
// exactly what only it knows: when the quiet window elapsed, when the max-wait
// ceiling hit, and whether the child is mid-turn.
//
// It does NOT hold the events. Producers push pre-rendered fragments, which
// are persisted to the inbox on the way through; when the buffer decides it is
// time, FlushFn asks the owner to deliver the rows. That split is the point: a
// daemon that dies inside the quiet window used to lose the batch outright,
// and coalescing recomputed from the rows re-coalesces correctly on restart.
//
// A zero-value Config is not usable; construct via New.
type Buffer struct {
	mu    sync.Mutex
	cfg   Config
	clk   Clock
	state map[bufKey]*perKey
	flush FlushFn
	busy  BusyFn
	acc   inbox.Accepter
}

// New returns a Buffer using the given clock. Config fields that are zero are
// set to documented defaults. The returned buffer has no delivery function;
// call SetFlush, SetBusy and SetAccepter before use.
func New(cfg Config, clk Clock) *Buffer {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 5 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 60 * time.Second
	}
	if cfg.MaxBytesPerFrag <= 0 {
		cfg.MaxBytesPerFrag = 8192
	}
	return &Buffer{
		cfg:   cfg,
		clk:   clk,
		state: make(map[bufKey]*perKey),
	}
}

// SetFlush registers the delivery callback. Must be called before any Push.
func (b *Buffer) SetFlush(fn FlushFn) {
	b.flush = fn
}

// SetBusy registers the busy check. Must be called before any Push whose
// target may be mid-turn.
func (b *Buffer) SetBusy(fn BusyFn) {
	b.busy = fn
}

// SetAccepter attaches the durable inbox. Until it is set, every Push is an
// orphan: still delivered, never persisted. That is the pre-durable-inbox
// behaviour and is what a unit test with no store gets.
func (b *Buffer) SetAccepter(a inbox.Accepter) { b.acc = a }

// Push records a fragment for (childID, source) and arms the debounce.
//
// The fragment is persisted to the inbox BEFORE the timer is armed. That
// ordering is the whole feature: a daemon that dies inside the quiet window
// used to lose the batch outright, which is how a coordinator ended up waiting
// forever for a settle that had already happened.
//
// key is last-write-wins WITHIN (childID, source); key == "" accumulates.
// Coalescing itself happens at delivery, over the rows.
//
// Source names must not contain "::". A source that does is rejected with a
// warning log and the fragment is silently dropped.
func (b *Buffer) Push(childID, source, key, fragment string) {
	if !validSource(childID, source, "Push") {
		return
	}
	b.accept(childID, source, key, fragment, inbox.ModePrompt)
	b.mu.Lock()
	b.armLocked(childID, source)
	b.mu.Unlock()
}

// PushNow bypasses the DEBOUNCE but not the busy gate: a child mid-turn keeps
// the batch until DrainIdle.
//
// Use it for news that cannot wait for the next quiet window but does not
// invalidate the turn in progress — a budget warning, a subagent failure the
// coordinator will act on next turn.
func (b *Buffer) PushNow(childID, source, fragment string) {
	if !validSource(childID, source, "PushNow") {
		return
	}
	b.accept(childID, source, "", fragment, inbox.ModePrompt)
	b.fire(bufKey{childID, source}, false)
}

// PushSteer bypasses BOTH the debounce and the busy gate, and is accepted as a
// steer so the batch delivers into the turn already running. Any pending
// fragment for the same source rides along, because Coalesce reads every
// pending row for the group — and a group holding any steer delivers as a
// steer, which is how stickiness survives as data rather than buffer state.
//
// Reserve it for events that invalidate that turn: executor lost, budget
// exhausted mid-tool-call.
func (b *Buffer) PushSteer(childID, source, fragment string) {
	if !validSource(childID, source, "PushSteer") {
		return
	}
	b.accept(childID, source, "", fragment, inbox.ModeSteer)
	b.fire(bufKey{childID, source}, true)
}

// DrainIdle flushes every batch DEFERRED because childID was busy. Call it on
// the child's transition to idle.
//
// It deliberately leaves batches still inside their debounce window alone: a
// fragment pushed 200ms before turn-end would otherwise be delivered on its
// own, costing exactly the extra turn this package exists to avoid.
func (b *Buffer) DrainIdle(childID string) {
	var keys []bufKey
	b.mu.Lock()
	for bk, pk := range b.state {
		if bk.childID == childID && pk.deferred && pk.dirty {
			keys = append(keys, bk)
		}
	}
	b.mu.Unlock()
	for _, bk := range keys {
		b.fire(bk, true)
	}
}

// Forget discards childID's SCHEDULING state and stops its timers.
//
// The daemon calls this on child exit. Without it a child that dies mid-turn
// leaves its deferred state in the buffer forever: DrainIdle is the only thing
// that clears it and it only fires on an idle transition, which a dead child
// never makes.
//
// It does NOT discard messages: rows that made it into the store outlive the
// buffer, and what happens to THEM on exit is the controller's decision
// (Reset on exit, Drop when the child is forgotten for good) — not this
// package's. Orphans are different: they never reached the store (the durable
// accept failed), so THIS is the only copy, and deleting the perKey without
// handing them anywhere would silently discard exactly the fragments the
// orphan path exists to save — a child dying right after a database blip
// would lose them with no error and no trace. So any orphans queued for
// childID are flushed to the owner exactly as a live fire would, before their
// scheduling state is dropped.
func (b *Buffer) Forget(childID string) {
	type forgotten struct {
		source  string
		orphans []inbox.Inbound
	}
	b.mu.Lock()
	var toFlush []forgotten
	for bk, pk := range b.state {
		if bk.childID != childID {
			continue
		}
		if pk.timer != nil {
			pk.timer.Stop()
		}
		if len(pk.orphans) > 0 {
			toFlush = append(toFlush, forgotten{source: bk.source, orphans: pk.orphans})
		}
		delete(b.state, bk)
	}
	flush := b.flush
	b.mu.Unlock()

	// flush MUST run with b.mu released — same reason as fire's: it reaches
	// Controller.Send, a blocking write, and a producer pushing from inside
	// that path would deadlock on a re-entrant acquire.
	if flush != nil {
		for _, f := range toFlush {
			flush(childID, f.source, f.orphans)
		}
	}
}

// --- internal ---

// acceptTimeout bounds the durable write on the push path. A producer here is
// often the daemon's own status goroutine, so this must not become a place a
// slow database stalls child bookkeeping.
const acceptTimeout = 5 * time.Second

func validSource(childID, source, op string) bool {
	if strings.Contains(source, "::") {
		slog.Warn("eventbuf: push rejected — source name contains \"::\"",
			"op", op, "childId", childID, "source", source)
		return false
	}
	return true
}

// accept persists one fragment. A store failure is logged and the whole
// message is retained in memory as an orphan: losing durability is bad, losing
// the notification is worse — and an orphan that lost its Mode on the way out
// is a steer that no longer interrupts anything.
func (b *Buffer) accept(childID, source, key, fragment string, mode inbox.Mode) {
	// ID and AcceptedAt stay empty: the store assigns those, and this row
	// never reached it.
	in := inbox.Inbound{
		ChildID: childID,
		Mode:    mode,
		Source:  source,
		Key:     key,
		Text:    truncate(fragment, b.cfg.MaxBytesPerFrag),
	}

	if b.acc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), acceptTimeout)
		_, err := b.acc.Accept(ctx, in)
		cancel()
		if err == nil {
			b.mu.Lock()
			b.keyLocked(bufKey{childID, source}).dirty = true
			b.mu.Unlock()
			return
		}
		slog.Warn("eventbuf: durable accept failed; delivering without durability",
			"childId", childID, "source", source, "mode", mode, "error", err)
	}

	b.mu.Lock()
	pk := b.keyLocked(bufKey{childID, source})
	pk.orphans = append(pk.orphans, in)
	pk.dirty = true
	b.mu.Unlock()
}

// keyLocked returns (creating if needed) the scheduling state for bk. Caller
// holds b.mu.
func (b *Buffer) keyLocked(bk bufKey) *perKey {
	pk := b.state[bk]
	if pk == nil {
		pk = &perKey{}
		b.state[bk] = pk
	}
	return pk
}

// armLocked (re)arms the debounce timer for (childID, source). Caller holds
// b.mu.
func (b *Buffer) armLocked(childID, source string) {
	now := b.clk.Now()
	bk := bufKey{childID, source}
	pk := b.keyLocked(bk)
	if pk.firstPushAt.IsZero() {
		pk.firstPushAt = now
	}
	if pk.timer != nil {
		pk.timer.Stop()
	}
	// Effective deadline: min(now+Debounce, firstPushAt+MaxWait).
	deadline := pk.firstPushAt.Add(b.cfg.MaxWait)
	if d := now.Add(b.cfg.Debounce); d.Before(deadline) {
		deadline = d
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}
	bkCopy := bk // capture for closure
	pk.timer = b.clk.AfterFunc(delay, func() { b.fire(bkCopy, false) })
}

// fire attempts a flush for bk. forced skips the busy gate (DrainIdle has
// already established the child is idle; PushSteer deliberately interrupts).
//
// The busy check and the deferral happen under ONE lock hold: checking outside
// the lock and re-acquiring to mark deferred leaves a window where a concurrent
// DrainIdle finds nothing deferred, and the batch then sits with no armed timer
// until the child's next busy->idle cycle.
func (b *Buffer) fire(bk bufKey, forced bool) {
	b.mu.Lock()
	pk := b.state[bk]
	if pk == nil || !pk.dirty {
		b.mu.Unlock()
		return
	}
	if !forced && b.busy != nil && b.busy(bk.childID) {
		pk.deferred = true
		b.mu.Unlock()
		return
	}
	orphans := pk.orphans
	pk.orphans = nil
	pk.dirty = false
	pk.deferred = false
	pk.firstPushAt = time.Time{}
	if pk.timer != nil {
		pk.timer.Stop()
		pk.timer = nil
	}
	flush := b.flush
	b.mu.Unlock()

	// flush MUST run with b.mu released: it reaches Controller.Send, a
	// blocking write to a child, and a producer pushing an event from inside
	// that path would deadlock on a re-entrant acquire.
	if flush != nil {
		flush(bk.childID, bk.source, orphans)
	}
}

// truncate cuts s to maxBytes with a visible "…(truncated)" suffix.
func truncate(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	suffix := "…(truncated)"
	cut := maxBytes - len(suffix)
	if cut < 0 {
		cut = 0
	}
	return s[:cut] + suffix
}
