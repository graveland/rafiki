// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Script output is published as durable ScriptOutput events, coalesced per
// child: a script's stdout is free-form text (ScriptProvider emits one
// script_output ParsedEvent per line), and publishing one event per line
// would flood the event log at line rate. Lines are coalesced — flush at
// 4 KiB or 250 ms, whichever first; never split a line unless the line alone
// exceeds 4 KiB, then split at 4 KiB — and the final flush runs on child exit
// BEFORE the child_exited event publishes, so a client backfilling from the
// event log sees the child's whole output before it sees the exit.
//
// Byte fidelity contract: every buffered line carries its own '\n' terminator
// and an oversized line's split pieces carry none, so CONCATENATING a
// stream's events in ordinal order reproduces the child's raw output on that
// stream — line-faithful for valid UTF-8 input. Fidelity holds for VALID UTF-8
// only: every line is sanitized at Add entry (see Add), and a mid-rune split
// would otherwise hand protobuf-go a proto3 string it refuses to marshal.

const (
	// scriptOutputFlushBytes is the buffered-size trigger: text buffered for
	// one stream flushes once it reaches this. An oversized LINE is split at
	// the last rune boundary ≤ this bound (the cut steps back up to 3 bytes;
	// see Add).
	scriptOutputFlushBytes = 4 * 1024

	// scriptOutputFlushAfter is the time trigger: 250 ms after a stream's
	// first unflushed line arrived, whatever is buffered flushes.
	scriptOutputFlushAfter = 250 * time.Millisecond
)

// scriptOutputStreams is the fixed flush order across a coalescer's streams —
// map iteration is randomized, and deterministic order keeps two streams'
// chunks from shuffling between runs. The values are the wire values carried
// in ScriptOutput.Stream.
var scriptOutputStreams = [...]string{"stdout", "stderr"}

// scriptOutChunk is one event's worth of text, ready to publish.
type scriptOutChunk struct {
	stream string
	text   string
}

// scriptOutStream is one stream's buffering state inside a coalescer.
//
// The split between units and buf is what keeps the no-split rule honest
// without depending on loop timing: units are sealed chunks that each become
// their OWN event (a size-sealed buffer; a 4 KiB piece of an oversized line),
// while buf is the open accumulator lines are still joining. Flushing emits
// every unit separately — so an oversized line can never re-merge with its
// neighbours into one oversized event, however backed up the flush loop is.
type scriptOutStream struct {
	// units are sealed chunks awaiting publish, in arrival order.
	units [][]byte
	// buf is the open accumulator: complete lines, each with its '\n',
	// coalescing toward the next seal.
	buf []byte
	// first is the arrival time of the stream's oldest unflushed OPEN-buffer
	// line; zero when nothing is unflushed (or when only sealed units remain —
	// units are always ready and anchor nothing). It anchors the 250 ms
	// deadline, and it is the OPEN buffer's anchor: a seal resets it, so the
	// lines that follow the seal flush 250 ms after THEIR arrival, not the
	// sealed content's.
	first time.Time
}

// sealLocked moves buf into units as one sealed chunk, clearing the 250 ms
// anchor with it: the anchor is the OPEN buffer's (units are always ready),
// so the lines that follow a seal time their flush from their own arrival,
// not the sealed content's. No-op when buf is empty.
func (st *scriptOutStream) sealLocked() {
	if len(st.buf) == 0 {
		return
	}
	st.units = append(st.units, st.buf)
	st.buf = make([]byte, 0, scriptOutputFlushBytes)
	st.first = time.Time{}
}

// emptyLocked reports whether the stream holds nothing unflushed.
func (st *scriptOutStream) emptyLocked() bool {
	return len(st.units) == 0 && len(st.buf) == 0
}

// scriptOutputClock abstracts the clock the coalescer reads, so
// TestScriptOutputCoalescesByTime can drive the 250 ms flush deterministically
// instead of sleeping real time.
type scriptOutputClock interface {
	Now() time.Time
	NewTimer(d time.Duration) scriptOutputTimer
}

// scriptOutputTimer is the one timer handle the coalescer needs.
type scriptOutputTimer interface {
	C() <-chan time.Time
	Stop()
}

// realScriptOutputClock is the production clock.
type realScriptOutputClock struct{}

func (realScriptOutputClock) Now() time.Time { return time.Now() }

func (realScriptOutputClock) NewTimer(d time.Duration) scriptOutputTimer {
	return realScriptOutputTimer{time.NewTimer(d)}
}

type realScriptOutputTimer struct{ t *time.Timer }

func (t realScriptOutputTimer) C() <-chan time.Time { return t.t.C }
func (t realScriptOutputTimer) Stop()               { t.t.Stop() }

// scriptOutputCoalescer coalesces one script child's raw output into durable
// ScriptOutput events, published through Controller.publishEvent (script_output
// is a durable type, so each flush gets an ordinal and is resumable).
//
// All flushing happens on the coalescer's own goroutine — Add only buffers and
// wakes — so events are serialized in arrival order and Close's final flush
// cannot interleave with a timed flush. Add never publishes: the caller is the
// child's stdout/stderr drain goroutine, and a publish (an event-log append
// with a 5 s timeout) must not stall the pipe.
type scriptOutputCoalescer struct {
	c       *Controller
	childID string
	clock   scriptOutputClock

	mu      sync.Mutex
	closed  bool
	streams map[string]*scriptOutStream

	// wake is a capacity-1 signal to the flush loop: buffered state changed,
	// recompute what is ready and how long the next sleep is. Non-blocking
	// sends; a pending signal covers any number of Adds.
	wake chan struct{}
	// done is closed by Close to tell the loop to flush everything and exit.
	done chan struct{}
	// stopped is closed by the loop when it has exited (after the final
	// flush); Close waits on it, so Close returning guarantees no publish is
	// still in flight and every buffered line has been emitted.
	stopped chan struct{}
}

// registerScriptOutputCoalescer creates the coalescer for childID and
// registers it, so handleChildExit can find and close it. Called lazily on a
// script child's FIRST line of output — a spawn that fails or prints nothing
// never creates one, so there is nothing to clean up on those paths. Returns
// nil when the child has already exited (the abandon path: a late line racing
// the exit), so no coalescer is ever registered after the take — the exited
// check and the registration share ONE critical section, closing the
// check-then-register window a two-step check would leave open. The check
// reads the closure's OWN state pointer, not the map: take deletes the map
// entry, so a map lookup after the take finds nothing and would wrongly
// register; the pointer is the only witness that survives the delete.
func (c *Controller) registerScriptOutputCoalescer(childID string, state *scriptOutputHookState) *scriptOutputCoalescer {
	co := &scriptOutputCoalescer{
		c:       c,
		childID: childID,
		clock:   realScriptOutputClock{},
		streams: make(map[string]*scriptOutStream),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	c.scriptOutputsMu.Lock()
	if state.exited {
		c.scriptOutputsMu.Unlock()
		return nil
	}
	if c.scriptOutputs == nil {
		c.scriptOutputs = make(map[string]*scriptOutputCoalescer)
	}
	c.scriptOutputs[childID] = co
	c.scriptOutputsMu.Unlock()
	go co.run()
	return co
}

// takeScriptOutputCoalescer removes and returns the child's coalescer, or nil
// when the child never produced output (and so never registered one). The
// removal marks the per-spawn state exited IN THE SAME CRITICAL SECTION and
// prunes it — the hook closure keeps its own pointer and
// registerScriptOutputCoalescer checks that pointer under the lock, so a late
// line racing the exit (the abandon path) sees exited and drops rather than
// registering a coalescer nobody would ever close; the delete is safe because
// nothing reads the map for exit status any more. The removal is also what
// makes re-registration safe on a later resume of the same child id: the old
// spawn's hook drops its own late lines, so it can never clobber the resumed
// spawn's registration.
func (c *Controller) takeScriptOutputCoalescer(childID string) *scriptOutputCoalescer {
	c.scriptOutputsMu.Lock()
	co := c.scriptOutputs[childID]
	delete(c.scriptOutputs, childID)
	if st, ok := c.scriptOutputState[childID]; ok {
		st.exited = true
		delete(c.scriptOutputState, childID)
	}
	c.scriptOutputsMu.Unlock()
	return co
}

// scriptOutputHookState is the per-spawn state scriptOutputHook closes over.
// It exists so the exit is visible to the hook: takeScriptOutputCoalescer
// flips exited in the same critical section it removes the registry entry.
// registerScriptOutputCoalescer reads it under scriptOutputsMu; nothing reads
// it on its own.
type scriptOutputHookState struct {
	exited bool
}

// scriptOutputHook returns the per-line hook a script child's SpawnSpec
// carries. The coalescer is created on the hook's first invocation, not at
// wiring time: childHooks runs before child.Spawn, and a spawn that fails
// there must leave nothing behind to clean up. Once the child has exited, the
// hook drops every line: no registration after exit, no publish after
// child_exited, no goroutine parked on a done nobody closes.
func (c *Controller) scriptOutputHook(childID string) func(stream, line string, terminated bool) {
	var once sync.Once
	var co *scriptOutputCoalescer
	state := &scriptOutputHookState{}
	c.scriptOutputsMu.Lock()
	if c.scriptOutputState == nil {
		c.scriptOutputState = make(map[string]*scriptOutputHookState)
	}
	c.scriptOutputState[childID] = state
	c.scriptOutputsMu.Unlock()
	return func(stream, line string, terminated bool) {
		once.Do(func() { co = c.registerScriptOutputCoalescer(childID, state) })
		// co is nil when the child had already exited by the time the first
		// line registered — including the take racing inside the registration's
		// critical section (the abandon path). Drop the line: no registration
		// after exit, no publish after child_exited, no goroutine parked on a
		// done nobody closes.
		if co == nil {
			return
		}
		co.Add(stream, line, terminated)
	}
}

// Add buffers one raw line of a script child's output — the line WITHOUT its
// terminator; the coalescer adds the '\n' when terminated. stream is
// "stdout" or "stderr"; the streams buffer independently. terminated=false
// (an unterminated FRAGMENT — an oversized line's piece, or readStderr's
// bounded accumulator handing off its first 4 KiB) seals as its own unit
// exactly like a split piece: no '\n' is added, so concatenating the events
// in order reproduces the raw stream. Fragments of one pending buffer arrive
// in sequence, so concatenation stays byte-faithful.
//
// The line is sanitized to valid UTF-8 at entry (strings.ToValidUTF8):
// proto3 string fields refuse to marshal invalid UTF-8, which would fail the
// durable append (the chunk lost) AND kill every live subscriber stream. Byte
// fidelity therefore holds for valid UTF-8 input only; invalid bytes become
// U+FFFD.
//
// A terminated line alone longer than 4 KiB is split at the last rune
// boundary ≤ 4 KiB — each piece becomes its own sealed unit, so each
// publishes as its own event — and its trailing remainder opens the buffer,
// to coalesce with the following lines. Any other line appends to the open
// buffer, sealing it BEFORE the append when the line would push it past the
// 4 KiB trigger (so a size-triggered event stays ≤ 4 KiB + one newline), and
// sealing it once it reaches the trigger after the append. Fragments are
// sealed as units without appending at all. Lines are dropped after Close:
// there is no flush after the final one.
func (s *scriptOutputCoalescer) Add(stream, line string, terminated bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	line = strings.ToValidUTF8(line, "\uFFFD")
	st := s.streams[stream]
	if st == nil {
		st = &scriptOutStream{buf: make([]byte, 0, scriptOutputFlushBytes)}
		s.streams[stream] = st
	}
	if !terminated {
		// A fragment is its own sealed unit, byte-faithful, no '\n' added —
		// the same shape an oversized split's pieces already had. The open
		// buffer seals FIRST: units publish immediately while buf waits for
		// its 250 ms deadline, so a fragment must not jump ahead of earlier
		// terminated lines still waiting there.
		st.sealLocked()
		st.units = append(st.units, []byte(line))
		s.mu.Unlock()
		s.wakeNonblock()
		return
	}
	if len(line) > scriptOutputFlushBytes {
		// Never merge an oversized line with anything else: seal whatever is
		// open, then split the line at the last rune boundary ≤ the bound
		// (each cut steps back up to 3 bytes to land on a rune start). The
		// pieces carry no newline — they are fragments of one line — and the
		// trailing remainder (1..4096 bytes, never empty) lands in the open
		// buffer with the line's terminator, ready to coalesce forward — then
		// the buffer is size-sealed if the remainder is ≥ 4096 bytes (the
		// newline that lands with it is what reaches the bound), exactly as
		// any other append is.
		st.sealLocked()
		for len(line) > scriptOutputFlushBytes {
			cut := scriptOutputFlushBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			st.units = append(st.units, []byte(line[:cut]))
			line = line[cut:]
		}
		st.buf = append(st.buf, line...)
		st.buf = append(st.buf, '\n')
		if len(st.buf) >= scriptOutputFlushBytes {
			st.sealLocked()
		}
	} else {
		// A line is never split by coalescing: seal the open buffer BEFORE
		// appending when this line would push it past the trigger, so a
		// size-triggered event is at most the trigger plus one newline.
		if len(st.buf)+len(line)+1 > scriptOutputFlushBytes {
			st.sealLocked()
		}
		if len(st.buf) == 0 {
			// First line of the open buffer: it anchors the 250 ms deadline
			// (a seal cleared any earlier anchor).
			st.first = s.clock.Now()
		}
		st.buf = append(st.buf, line...)
		st.buf = append(st.buf, '\n')
		if len(st.buf) >= scriptOutputFlushBytes {
			st.sealLocked()
		}
	}
	s.mu.Unlock()
	s.wakeNonblock()
}

// wakeNonblock signals the flush loop that buffered state changed.
// Non-blocking: a pending signal covers any number of Adds.
func (s *scriptOutputCoalescer) wakeNonblock() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close flushes both streams and stops the flush loop. It blocks until the
// final flush has published, so the caller (handleChildExit, before the exit
// event) can rely on the child's whole output preceding child_exited in
// ordinal order. Idempotent; the second Close just waits for the same exit.
func (s *scriptOutputCoalescer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.stopped
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.done)
	<-s.stopped
}

// run is the flush loop, and the only publisher: Add buffers and wakes, it
// never publishes, so every event this child emits is serialized on THIS
// goroutine in arrival order — which is what makes Close's final flush the
// last one and keeps "output precedes child_exited" true without an ordering
// protocol. Each iteration publishes everything that is ready — sealed units
// always (they exist only because a size trigger fired), the open buffer when
// its 250 ms deadline has passed — then parks until the next wake, deadline
// or Close. The publish itself runs OUTSIDE mu (an event-log append with a
// 5 s timeout must not stall Add), so ordering rests on the single-publisher
// rule above, not on the lock: mu coordinates buffer state between Add and
// this loop, nothing else.
func (s *scriptOutputCoalescer) run() {
	defer close(s.stopped)
	for {
		now := s.clock.Now()
		s.mu.Lock()
		if s.closed {
			// Final flush: everything unflushed, both streams, then exit. No
			// Add can add anything after this point.
			chunks := s.takeAllLocked()
			s.mu.Unlock()
			for _, ch := range chunks {
				s.publish(ch)
			}
			return
		}
		ready := s.takeReadyLocked(now)
		deadline := s.nextDeadlineLocked()
		s.mu.Unlock()
		for _, ch := range ready {
			s.publish(ch)
		}
		if len(ready) > 0 {
			// More may have become ready while publishing (and Close may have
			// been called): re-evaluate from the top.
			continue
		}

		var timer scriptOutputTimer
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			d := deadline.Sub(now)
			if d < 0 {
				d = 0
			}
			timer = s.clock.NewTimer(d)
			timerC = timer.C()
		}
		select {
		case <-s.done:
			// Close: loop back so the closed branch above does the final
			// flush. The timer is stopped — it must never fire again.
			if timer != nil {
				timer.Stop()
			}
		case <-s.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
			// Deadline reached: loop back and flush what is due.
		}
	}
}

// takeReadyLocked takes every chunk ready to publish at now: sealed units
// always (a unit exists only because its size triggered), plus the open
// buffer of any stream whose 250 ms deadline has passed. A stream's deadline
// anchor clears only when the stream is fully drained.
// s.mu must be held.
func (s *scriptOutputCoalescer) takeReadyLocked(now time.Time) []scriptOutChunk {
	var chunks []scriptOutChunk
	for _, stream := range scriptOutputStreams {
		st := s.streams[stream]
		if st == nil {
			continue
		}
		due := !st.first.IsZero() && !st.first.Add(scriptOutputFlushAfter).After(now)
		if len(st.units) == 0 && !due {
			continue
		}
		for _, u := range st.units {
			chunks = append(chunks, scriptOutChunk{stream: stream, text: string(u)})
		}
		st.units = nil
		if due {
			if len(st.buf) > 0 {
				chunks = append(chunks, scriptOutChunk{stream: stream, text: string(st.buf)})
				st.buf = st.buf[:0]
			}
		}
		if st.emptyLocked() {
			// Nothing unflushed left: the anchor must clear, or a stale past
			// deadline would spin the loop.
			st.first = time.Time{}
		}
	}
	return chunks
}

// nextDeadlineLocked returns the earliest flush deadline across the streams'
// unflushed content, or zero when nothing is unflushed.
// s.mu must be held.
func (s *scriptOutputCoalescer) nextDeadlineLocked() time.Time {
	var earliest time.Time
	for _, stream := range scriptOutputStreams {
		st := s.streams[stream]
		if st == nil || st.first.IsZero() {
			continue
		}
		d := st.first.Add(scriptOutputFlushAfter)
		if earliest.IsZero() || d.Before(earliest) {
			earliest = d
		}
	}
	return earliest
}

// takeAllLocked takes everything unflushed, both streams, in flush order.
// s.mu must be held.
func (s *scriptOutputCoalescer) takeAllLocked() []scriptOutChunk {
	var chunks []scriptOutChunk
	for _, stream := range scriptOutputStreams {
		st := s.streams[stream]
		if st == nil {
			continue
		}
		for _, u := range st.units {
			chunks = append(chunks, scriptOutChunk{stream: stream, text: string(u)})
		}
		st.units = nil
		if len(st.buf) > 0 {
			chunks = append(chunks, scriptOutChunk{stream: stream, text: string(st.buf)})
			st.buf = st.buf[:0]
		}
		st.first = time.Time{}
	}
	return chunks
}

// publish emits one ScriptOutput event through the ONE native-event path.
// Durable tier, so publishEvent assigns the ordinal that orders this child's
// output against its child_exited event.
func (s *scriptOutputCoalescer) publish(ch scriptOutChunk) {
	now := s.clock.Now()
	s.c.publishEvent(s.childID, &rafikiv1.Event{
		ChildId: s.childID,
		Ts:      timestamppb.New(now),
		Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
			Stream: ch.stream,
			Text:   ch.text,
		}},
	})
}
