package eventbuf

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Config controls event buffer behaviour. Zero values are replaced with
// sensible defaults by New.
type Config struct {
	Debounce         time.Duration // quiet window after last push before flush (default 5s)
	MaxWait          time.Duration // ceiling measured from first push (default 60s)
	MaxFragments     int           // max fragments per batch (default 30)
	MaxBytesPerFrag  int           // max bytes per fragment before truncation (default 8192)
	MaxBytesPerFlush int           // max total bytes per flush (default 65536)
}

// Delivery says HOW a batch reaches the child. It is independent of WHEN:
// the debounce decides when, this decides which frame kind carries it.
//
// Keeping them separate is the whole point. PushNow skips the debounce but
// still waits for the turn to end (DeliverPrompt); PushSteer does not wait
// (DeliverSteer). Welding "urgent" to "steer" makes every urgent event
// interrupt a turn it may have nothing to do with.
type Delivery int

const (
	// DeliverPrompt queues the batch; the child sees it when its next turn
	// starts. This is the default for everything.
	DeliverPrompt Delivery = iota
	// DeliverSteer injects the batch into the turn already running. Reserve
	// it for events that INVALIDATE that turn — a worker that has lost its
	// executor must not spend another 40s believing it still has one.
	DeliverSteer
)

func (d Delivery) String() string {
	if d == DeliverSteer {
		return "steer"
	}
	return "prompt"
}

// FlushFn delivers a coalesced batch. childID and source identify the target;
// d selects the frame kind.
type FlushFn func(childID, source string, fragments []string, d Delivery)

// BusyFn reports whether childID is mid-turn. A flush is deferred while true.
type BusyFn func(childID string) bool

type bufKey struct {
	childID string
	source  string
}

type perKey struct {
	keyed           map[string]string // key -> latest fragment (last-write-wins)
	keyOrder        []string          // insertion order, for stable output
	unkeyed         []string
	firstPushAt     time.Time
	timer           Timer
	deferred        bool     // a flush was withheld because the child was busy
	pendingDelivery Delivery // sticky steer: a deferred steer stays a steer
}

// Buffer coalesces externally-injected agent events into debounced, batched
// frames. Producers push pre-rendered fragments; a debounce timer with a
// max-wait ceiling flushes them as one delivery via the configured FlushFn.
// A flush is deferred while the child is mid-turn and released on its idle
// transition via DrainIdle.
//
// A zero-value Config is not usable; construct via New.
type Buffer struct {
	mu    sync.Mutex
	cfg   Config
	clk   Clock
	state map[bufKey]*perKey
	flush FlushFn
	busy  BusyFn
}

// New returns a Buffer using the given clock. Config fields that are zero are
// set to documented defaults. The returned buffer has no delivery function;
// call SetFlush and SetBusy before use.
func New(cfg Config, clk Clock) *Buffer {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 5 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 60 * time.Second
	}
	if cfg.MaxFragments <= 0 {
		cfg.MaxFragments = 30
	}
	if cfg.MaxBytesPerFrag <= 0 {
		cfg.MaxBytesPerFrag = 8192
	}
	if cfg.MaxBytesPerFlush <= 0 {
		cfg.MaxBytesPerFlush = 65536
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

// pending is one assembled batch waiting to be handed to the flush callback
// after the lock is released.
type pending struct {
	bk        bufKey
	fragments []string
	delivery  Delivery
	forced    bool // skip the busy gate in emit
}

// Push adds a fragment to the buffer for (childID, source). When key is
// non-empty subsequent pushes with the same key overwrite (last-write-wins);
// key == "" means the fragment accumulates. The fragment is truncated to
// Config.MaxBytesPerFrag with a visible marker.
//
// Delivery is debounced and prompt, and defers while the child is mid-turn.
//
// Source names must not contain "::". A source that does is rejected with a
// warning log and the fragment is silently dropped.
func (b *Buffer) Push(childID, source, key, fragment string) {
	if !validSource(childID, source, "Push") {
		return
	}
	b.mu.Lock()
	b.appendLocked(childID, source, key, truncate(fragment, b.cfg.MaxBytesPerFrag))
	b.mu.Unlock()
}

// PushNow bypasses the DEBOUNCE: the fragment joins any pending batch and the
// whole thing is delivered at once instead of waiting out the quiet window.
//
// It does NOT bypass the busy gate. A child mid-turn keeps the batch until
// DrainIdle. Use it for news that cannot wait for the next quiet window but
// does not invalidate the turn in progress — a budget warning, a subagent
// failure the coordinator will act on next turn.
func (b *Buffer) PushNow(childID, source, fragment string) {
	if !validSource(childID, source, "PushNow") {
		return
	}
	b.mu.Lock()
	b.appendLocked(childID, source, "", truncate(fragment, b.cfg.MaxBytesPerFrag))
	p := b.takeLocked(bufKey{childID, source}, DeliverPrompt)
	b.mu.Unlock()
	b.emit(p)
}

// PushSteer bypasses BOTH the debounce and the busy gate, and delivers as a
// steer — injected into the turn already running.
//
// Reserve it for events that invalidate that turn: executor lost, budget
// exhausted mid-tool-call. Any pending batch for the same source rides along
// so ordering is preserved.
func (b *Buffer) PushSteer(childID, source, fragment string) {
	if !validSource(childID, source, "PushSteer") {
		return
	}
	b.mu.Lock()
	b.appendLocked(childID, source, "", truncate(fragment, b.cfg.MaxBytesPerFrag))
	p := b.takeLocked(bufKey{childID, source}, DeliverSteer)
	if p != nil {
		p.forced = true
	}
	b.mu.Unlock()
	b.emit(p)
}

// DrainIdle flushes every batch that was DEFERRED because childID was busy.
// Call this on the child's transition to idle.
//
// It deliberately leaves batches still inside their debounce window alone: a
// fragment pushed 200ms before turn-end would otherwise be delivered on its
// own, costing exactly the extra turn this package exists to avoid.
func (b *Buffer) DrainIdle(childID string) {
	var out []*pending
	b.mu.Lock()
	for bk, pk := range b.state {
		if bk.childID != childID || !pk.deferred || pk.isEmpty() {
			continue
		}
		if p := b.takeLocked(bk, pk.pendingDelivery); p != nil {
			p.forced = true
			out = append(out, p)
		}
	}
	b.mu.Unlock()
	for _, p := range out {
		b.emit(p)
	}
}

// Forget discards every buffered batch for childID and stops its timers.
//
// The daemon calls this on child exit. Without it a child that dies mid-turn
// leaves its deferred batches in the buffer forever: DrainIdle is the only
// thing that clears deferred state and it only fires on an idle transition,
// which a dead child never makes.
func (b *Buffer) Forget(childID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for bk, pk := range b.state {
		if bk.childID != childID {
			continue
		}
		pk.reset()
		delete(b.state, bk)
	}
}

// --- internal ---

func validSource(childID, source, op string) bool {
	if strings.Contains(source, "::") {
		slog.Warn("eventbuf: push rejected — source name contains \"::\"",
			"op", op, "childId", childID, "source", source)
		return false
	}
	return true
}

// appendLocked records a fragment and (re)arms the debounce timer. Caller
// must hold b.mu.
func (b *Buffer) appendLocked(childID, source, key, fragment string) {
	now := b.clk.Now()
	bk := bufKey{childID, source}
	pk := b.state[bk]
	if pk == nil {
		pk = &perKey{keyed: make(map[string]string)}
		b.state[bk] = pk
	}

	if pk.firstPushAt.IsZero() {
		pk.firstPushAt = now
	}

	if key != "" {
		if _, exists := pk.keyed[key]; !exists {
			pk.keyOrder = append(pk.keyOrder, key)
		}
		pk.keyed[key] = fragment
	} else {
		pk.unkeyed = append(pk.unkeyed, fragment)
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
	pk.timer = b.clk.AfterFunc(delay, func() {
		b.timerFired(bkCopy)
	})
}

// takeLocked assembles and CLEARS the batch for bk, returning it for delivery
// after the lock is released. Returns nil when there is nothing to send.
// Caller must hold b.mu.
func (b *Buffer) takeLocked(bk bufKey, d Delivery) *pending {
	pk := b.state[bk]
	if pk == nil || pk.isEmpty() {
		return nil
	}
	p := &pending{bk: bk, fragments: b.assembleBatch(pk), delivery: d}
	pk.reset()
	pk.deferred = false
	pk.pendingDelivery = DeliverPrompt
	return p
}

// emit calls the flush callback. It MUST run with b.mu released: flush is
// Controller.injectBatch → Controller.Send, a blocking write to a child, and
// a producer that pushes an event from inside that path would otherwise
// deadlock on a re-entrant acquire of b.mu.
func (b *Buffer) emit(p *pending) {
	if p == nil || b.flush == nil {
		return
	}
	// A forced delivery skips the busy gate (DrainIdle already established
	// the child is idle; PushSteer deliberately interrupts).
	//
	// The check and the redeposit happen under ONE lock hold — checking
	// b.busy outside the lock and re-acquiring only to redeposit leaves a
	// window where a concurrent DrainIdle can run between the two, find
	// nothing deferred yet, and miss this batch; it would then be marked
	// deferred after the idle transition already passed and sit stranded
	// with no armed timer until the child's next busy→idle cycle.
	if !p.forced && b.busy != nil {
		b.mu.Lock()
		if b.busy(p.bk.childID) {
			b.redepositLocked(p)
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
	}
	b.flush(p.bk.childID, p.bk.source, p.fragments, p.delivery)
}

// redepositLocked puts an undeliverable batch back and marks it deferred so
// DrainIdle picks it up. Caller must hold b.mu.
func (b *Buffer) redepositLocked(p *pending) {
	pk := b.state[p.bk]
	if pk == nil {
		pk = &perKey{keyed: make(map[string]string)}
		b.state[p.bk] = pk
	}
	// Prepend: this batch was assembled before anything pushed since.
	pk.unkeyed = append(append([]string{}, p.fragments...), pk.unkeyed...)
	pk.deferred = true
	if p.delivery == DeliverSteer {
		pk.pendingDelivery = DeliverSteer
	}
}

func (b *Buffer) timerFired(bk bufKey) {
	b.mu.Lock()
	pk := b.state[bk]
	if pk == nil || pk.isEmpty() {
		b.mu.Unlock()
		return
	}
	if b.busy != nil && b.busy(bk.childID) {
		pk.deferred = true
		b.mu.Unlock()
		return
	}
	p := b.takeLocked(bk, pk.pendingDelivery)
	if p != nil {
		p.forced = true // the busy check above already passed
	}
	b.mu.Unlock()
	b.emit(p)
}

// assembleBatch builds the fragment list from a perKey, applying
// MaxFragments and THEN MaxBytesPerFlush with visible truncation markers.
// Both caps apply: an early return from the fragment cap would skip the byte
// budget exactly when the batch is largest.
func (b *Buffer) assembleBatch(pk *perKey) []string {
	var all []string
	for _, k := range pk.keyOrder {
		all = append(all, pk.keyed[k])
	}
	all = append(all, pk.unkeyed...)

	total := len(all)
	omitted := 0

	if b.cfg.MaxFragments > 0 && total > b.cfg.MaxFragments {
		kept := b.cfg.MaxFragments - 1
		if kept < 0 {
			kept = 0
		}
		omitted = total - kept
		all = all[:kept]
	}

	if b.cfg.MaxBytesPerFlush > 0 {
		used := 0
		for i, s := range all {
			if used+len(s) > b.cfg.MaxBytesPerFlush {
				omitted += len(all) - i
				all = all[:i]
				break
			}
			used += len(s)
		}
	}

	if omitted > 0 {
		all = append(all, fmt.Sprintf("[%d fragment(s) omitted]", omitted))
	}
	return all
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

func (pk *perKey) isEmpty() bool {
	return len(pk.keyed) == 0 && len(pk.unkeyed) == 0
}

func (pk *perKey) reset() {
	pk.keyed = make(map[string]string)
	pk.keyOrder = nil
	pk.unkeyed = nil
	pk.firstPushAt = time.Time{}
	if pk.timer != nil {
		pk.timer.Stop()
		pk.timer = nil
	}
}
