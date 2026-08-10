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

// FlushFn delivers a coalesced batch. childID and source identify the target;
// urgent is true when PushNow triggered the flush.
type FlushFn func(childID, source string, fragments []string, urgent bool)

// BusyFn reports whether childID is mid-turn. A flush is deferred while true.
type BusyFn func(childID string) bool

type bufKey struct {
	childID string
	source  string
}

type perKey struct {
	keyed       map[string]string // key -> latest fragment (last-write-wins)
	keyOrder    []string          // insertion order, for stable output
	unkeyed     []string
	firstPushAt time.Time
	timer       Timer
	deferred    bool // a flush fired but the child was busy
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

// Push adds a fragment to the buffer for (childID, source). When key is
// non-empty subsequent pushes with the same key overwrite (last-write-wins);
// key == "" means the fragment accumulates. The fragment is truncated to
// Config.MaxBytesPerFrag with a visible marker.
//
// Source names must not contain "::". A source that does is rejected with a
// warning log and the fragment is silently dropped.
func (b *Buffer) Push(childID, source, key, fragment string) {
	if strings.Contains(source, "::") {
		slog.Warn("eventbuf: push rejected — source name contains \"::\"", "childId", childID, "source", source)
		return
	}
	b.push(childID, source, key, truncate(fragment, b.cfg.MaxBytesPerFrag))
}

// PushNow bypasses the debounce. It drains any pending batch for (childID,
// source) first so ordering is preserved, then delivers the urgent fragment
// immediately. Still honours the busy gate.
func (b *Buffer) PushNow(childID, source, fragment string) {
	if strings.Contains(source, "::") {
		slog.Warn("eventbuf: PushNow rejected — source name contains \"::\"", "childId", childID, "source", source)
		return
	}
	frag := truncate(fragment, b.cfg.MaxBytesPerFrag)
	b.mu.Lock()
	bk := bufKey{childID, source}
	pk := b.state[bk]
	if pk != nil && !pk.isEmpty() {
		// Drain the existing batch first, without urgency.
		b.flushLocked(bk, false)
	}
	b.mu.Unlock()

	// Deliver the urgent fragment immediately.
	if b.flush != nil {
		b.flush(childID, source, []string{frag}, true)
	}
	// If the urgent flush was deferred (busy=true), drain the batch state
	// now so the idle transition does not re-deliver it.
	b.mu.Lock()
	if pk := b.state[bk]; pk != nil && pk.isEmpty() {
		delete(b.state, bk)
	}
	b.mu.Unlock()
}

// DrainIdle flushes every batch that was deferred because childID was busy.
// Call this on the child's transition to idle.
func (b *Buffer) DrainIdle(childID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for bk, pk := range b.state {
		if bk.childID == childID && !pk.isEmpty() {
			b.flushLocked(bk, false)
		}
	}
}

// --- internal ---

func (b *Buffer) push(childID, source, key, fragment string) {
	b.mu.Lock()
	defer b.mu.Unlock()

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

	// Re-arm the timer for the effective deadline.
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

func (b *Buffer) timerFired(bk bufKey) {
	b.mu.Lock()
	defer b.mu.Unlock()

	pk := b.state[bk]
	if pk == nil || pk.isEmpty() {
		return
	}

	if b.busy != nil && b.busy(bk.childID) {
		pk.deferred = true
		return
	}

	b.flushLocked(bk, false)
}

// flushLocked assembles fragments, clears state, and calls flush outside the
// lock. The caller must hold b.mu.
func (b *Buffer) flushLocked(bk bufKey, urgent bool) {
	pk := b.state[bk]
	if pk == nil || pk.isEmpty() {
		return
	}

	fragments := b.assembleBatch(pk)
	pk.reset()
	pk.deferred = false

	if b.flush != nil {
		b.flush(bk.childID, bk.source, fragments, urgent)
	}
}

// assembleBatch builds the fragment list from a perKey, applying
// MaxFragments and MaxBytesPerFlush with visible truncation markers.
func (b *Buffer) assembleBatch(pk *perKey) []string {
	var all []string
	// Gather in insertion order: keyed then unkeyed.
	for _, k := range pk.keyOrder {
		all = append(all, pk.keyed[k])
	}
	all = append(all, pk.unkeyed...)

	total := len(all)

	// Apply MaxFragments: if we have more than MaxFragments, keep at most
	// MaxFragments-1 real fragments and reserve the last slot for the
	// omission marker.
	if b.cfg.MaxFragments > 0 && total > b.cfg.MaxFragments {
		// Keep MaxFragments-1 real fragments, replace rest with marker.
		kept := b.cfg.MaxFragments - 1
		if kept < 0 {
			kept = 0
		}
		all = all[:kept]
		all = append(all, fmt.Sprintf("[%d fragment(s) omitted]", total-kept))
		return all
	}

	// Apply MaxBytesPerFlush: accumulate until we exceed the budget.
	if b.cfg.MaxBytesPerFlush > 0 {
		var buf strings.Builder
		var frags []string
		for _, s := range all {
			if buf.Len()+len(s) > b.cfg.MaxBytesPerFlush {
				remaining := total - len(frags)
				if remaining > 0 {
					frags = append(frags, fmt.Sprintf("[%d fragment(s) omitted]", remaining))
				}
				return frags
			}
			frags = append(frags, s)
			buf.WriteString(s)
		}
		return frags
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
