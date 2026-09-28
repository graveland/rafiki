// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"

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
// stream exactly.

const (
	// scriptOutputFlushBytes is the buffered-size trigger: text buffered for
	// one stream flushes once it reaches this. An oversized LINE is split at
	// exactly this bound (see Add).
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
	// first is the arrival time of the stream's oldest unflushed line; zero
	// when nothing is unflushed. It anchors the 250 ms deadline.
	first time.Time
}

// sealLocked moves buf into units as one sealed chunk. No-op when buf is
// empty.
func (st *scriptOutStream) sealLocked() {
	if len(st.buf) == 0 {
		return
	}
	st.units = append(st.units, st.buf)
	st.buf = make([]byte, 0, scriptOutputFlushBytes)
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
// never creates one, so there is nothing to clean up on those paths.
func (c *Controller) registerScriptOutputCoalescer(childID string) *scriptOutputCoalescer {
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
// removal is what makes re-registration safe on a later resume of the same
// child id.
func (c *Controller) takeScriptOutputCoalescer(childID string) *scriptOutputCoalescer {
	c.scriptOutputsMu.Lock()
	co := c.scriptOutputs[childID]
	delete(c.scriptOutputs, childID)
	c.scriptOutputsMu.Unlock()
	return co
}

// scriptOutputHook returns the per-line hook a script child's SpawnSpec
// carries. The coalescer is created on the hook's first invocation, not at
// wiring time: childHooks runs before child.Spawn, and a spawn that fails
// there must leave nothing behind to clean up.
func (c *Controller) scriptOutputHook(childID string) func(stream, line string) {
	var once sync.Once
	var co *scriptOutputCoalescer
	return func(stream, line string) {
		once.Do(func() { co = c.registerScriptOutputCoalescer(childID) })
		co.Add(stream, line)
	}
}

// Add buffers one raw line of a script child's output — the line WITHOUT its
// terminator; the coalescer adds the '\n'. stream is "stdout" or "stderr";
// the streams buffer independently.
//
// A line alone longer than 4 KiB is split at 4 KiB — each piece becomes its
// own sealed unit, so each publishes as its own event — and its trailing
// remainder opens the buffer, to coalesce with the following lines. Any other
// line appends to the open buffer, sealing the buffer as a unit once it
// reaches the 4 KiB trigger. Lines are dropped after Close: there is no flush
// after the final one.
func (s *scriptOutputCoalescer) Add(stream, line string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	st := s.streams[stream]
	if st == nil {
		st = &scriptOutStream{buf: make([]byte, 0, scriptOutputFlushBytes)}
		s.streams[stream] = st
	}
	if st.first.IsZero() {
		st.first = s.clock.Now()
	}
	if len(line) > scriptOutputFlushBytes {
		// Never merge an oversized line with anything else: seal whatever is
		// open, then split the line at the threshold. The pieces carry no
		// newline — they are fragments of one line — and the trailing
		// remainder (which may be empty) lands in the open buffer with the
		// line's terminator, ready to coalesce forward.
		st.sealLocked()
		for len(line) > scriptOutputFlushBytes {
			st.units = append(st.units, []byte(line[:scriptOutputFlushBytes]))
			line = line[scriptOutputFlushBytes:]
		}
		st.buf = append(st.buf, line...)
		st.buf = append(st.buf, '\n')
	} else {
		st.buf = append(st.buf, line...)
		st.buf = append(st.buf, '\n')
		if len(st.buf) >= scriptOutputFlushBytes {
			st.sealLocked()
		}
	}
	s.mu.Unlock()
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
		ChildId:  s.childID,
		TsUnixMs: now.UnixMilli(),
		Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
			Stream: ch.stream,
			Text:   ch.text,
		}},
	})
}
