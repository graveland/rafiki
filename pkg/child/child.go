package child

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/bus"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
)

// SpawnSpec describes how to launch a child process.
type SpawnSpec struct {
	ChildID     string
	Cwd         string
	PiBinary    string   // resolved binary path (pi or claude, per Provider)
	Argv        []string // full argv excluding PiBinary itself
	ExtraArgs   []string // appended after Argv; useful in tests
	Env         []string // env vars for the child process; see EnvOverride
	EnvOverride bool     // if true, Env replaces the parent env entirely; if false, Env is appended to os.Environ()

	// Provider selects the wire protocol. When nil, PiProvider{} is used so
	// existing callers and tests keep pi behavior unchanged.
	Provider ProtocolProvider

	// Runner overrides how the child executes. When nil, a subprocess runner is
	// built from PiBinary/Argv/Env. Injected rather than selected inside this
	// package because internal/fundi imports it.
	Runner Runner

	// NativeSink, when non-nil, receives rafiki-native events translated from
	// this child's stdout. Only the claude provider translates native events
	// today; pi and fundi children use their own paths.
	NativeSink func(*rafikiv1.Event)
}

// ShutdownResult records the outcome of a graceful-shutdown sequence.
type ShutdownResult struct {
	ExitCode  int
	Signal    string
	Escalated bool // true if SIGTERM or SIGKILL was required
	// Abandoned is true when the terminal Kill() rung expired without the
	// runner ever being reaped: the child was declared dead without proof and
	// its execution context was left behind inside the daemon (see
	// abandonTimeout and Child.abandon). "Reaped" and "abandoned" are NOT
	// interchangeable outcomes — a caller, and an operator, must be able to
	// tell them apart.
	Abandoned bool
	Duration  time.Duration
}

// abandonTimeout bounds the terminal `<-c.done` wait in Shutdown — how long we
// wait for runner.Wait() to return AFTER Kill() has already cancelled the
// child's context and closed its pipe ends.
//
// Why it must be bounded at all: for a subprocess it need not be, because
// SIGKILL always lands and the reap is immediate. For an in-process child
// (internal/inproc) Kill() cancels a context and closes two pipe ends, which
// covers the wedged-*write* case it was written for — but eng.Wait() can still
// be parked on a turn blocked in a syscall no context can interrupt (a tool
// subprocess ignoring signals, an HTTP call with no timeout). An unbounded
// wait there hangs `fundi kill` outright, makes ShutdownAllChildren return at
// its 180s global bound with the goroutine still live, and can extend
// pool.Close() at daemon exit.
//
// Why 10 seconds:
//
//   - Long enough that a genuinely-terminating child is never abandoned. Once
//     Kill() has landed, all that remains is eng.Wait() (whose counts the turn
//     worker retires before it can park), eng.Close(), and the runtime's
//     shutdown func — MCP teardown, itself running on the very context Kill
//     just cancelled. That is milliseconds in practice, so 10s is three to
//     four orders of magnitude of headroom.
//   - Short enough to keep `fundi kill` responsive: it is additive to the
//     caller's own ladder, and 10s on top of a 30s kill rung is noise next to
//     the alternative of never returning at all.
//   - It keeps the whole per-child ladder inside the daemon's global bound:
//     120s stdin-close + 30s kill + 10s abandon = 160s < the 180s
//     globalTimeout in cmd/rafikid/main.go, so ShutdownAllChildren still
//     reports every child's outcome instead of tripping its own deadline first
//     and reporting nothing.
const abandonTimeout = 10 * time.Second

// inBufMaxFrames / inBufMaxBytes are the eviction caps for the stdin capture.
const (
	inBufMaxFrames = 1000
	inBufMaxBytes  = 16 << 20 // 16 MiB
)

// inBuffer is a bounded FIFO of raw stdin frames sent to the child. Oldest
// entries are dropped when either the frame count or the total byte size
// exceeds the configured caps. Used by log dumps (LogDumper.Dump).
type inBuffer struct {
	mu     sync.Mutex
	frames [][]byte
	total  int
}

func (b *inBuffer) append(frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.frames) >= inBufMaxFrames || (len(b.frames) > 0 && b.total+len(cp) > inBufMaxBytes) {
		b.total -= len(b.frames[0])
		b.frames = b.frames[1:]
	}
	// A single frame larger than the cap would bypass the eviction loop above
	// (len(b.frames)==0 short-circuits the byte check). Drop it instead of
	// storing an unbounded frame.
	if len(cp) > inBufMaxBytes {
		return
	}
	b.frames = append(b.frames, cp)
	b.total += len(cp)
}

func (b *inBuffer) snapshot() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, len(b.frames))
	for i, f := range b.frames {
		cp := make([]byte, len(f))
		copy(cp, f)
		out[i] = cp
	}
	return out
}

// Child manages one pi child process: its I/O pipes, an event bus, a ring
// buffer, and a graceful-shutdown sequence. All exported methods are safe to
// call concurrently. The supervise goroutine is the only writer to the child's
// stdin; callers queue frames via Send.
type Child struct {
	ID     string
	spec   SpawnSpec
	runner Runner

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	cmdCh chan []byte
	ready chan struct{} // closed once supervise loop is processing
	// done is closed when the child has exited and been reaped — or, on the
	// abandon path, when Shutdown gave up waiting for a reap that is never
	// coming (see abandon). Both closers go through closeDone, because a
	// leaked supervise goroutine that later completes would otherwise
	// double-close it and panic the daemon.
	done        chan struct{}
	doneOnce    sync.Once
	processDone chan struct{} // internal: closed by readStdout after Wait()

	exit ShutdownResult

	bus        *bus.Bus[[]byte]
	ring       *ring.Ring
	renderRing *ring.Ring // bus-frame capture for normalizing providers (claude); nil otherwise
	in         inBuffer   // bounded stdin frame capture for log dumps

	// errMu guards errBuf. readStderr is its only writer and StderrSnapshot
	// its only reader, and before the abandon path existed "call it after
	// Done() is closed" was a sufficient contract: Done() implied wg.Wait(),
	// which implied readStderr had returned. Shutdown can now close Done()
	// while a leaked reader goroutine is still live, so the ordering argument
	// no longer holds and the buffer needs a real lock.
	errMu  sync.Mutex
	errBuf bytes.Buffer

	mu     sync.Mutex
	closed bool // set by readStdout after cmd.Process.Wait() returns
	// abandoned is set by abandon() and means "this child's exit has already
	// been recorded without a reap". It makes the record final: a leaked
	// readStdout whose runner.Wait() eventually returns must not overwrite the
	// abandoned outcome with a late, now-meaningless exit status.
	abandoned bool

	// abandonAfter is the bound Shutdown applies to its terminal post-Kill
	// wait. It is a field rather than a direct read of abandonTimeout purely so
	// a test can shrink it: the mechanism under test is "the wait is bounded
	// and abandonment is safe", not "ten seconds elapse". Spawn is its only
	// production writer, and Shutdown its only reader.
	abandonAfter time.Duration

	sm         *StateMachine
	provider   ProtocolProvider
	nativeSink func(*rafikiv1.Event)
	// preShutdownStatus is the status before BeginShutdown was called, captured
	// so handleChildExit can record the child's real pre-exit state (idle,
	// streaming, etc.) rather than "shutting_down" which is an artifact of the
	// daemon's own graceful-shutdown sequence. Read via PreShutdownStatus(); set
	// by BeginShutdown under metaMu.
	preShutdownStatus protocol.Status
	// metaMu protects meta, sm and transitions to allow Status()/Metadata()
	// concurrent reads.
	metaMu sync.Mutex
	meta   SnifferMetadata
	// transitions is the queue of status changes the state machine has made but
	// nobody has consumed yet, appended under metaMu — the lock that already
	// guards the SM, so a transition and its record are one atomic step — and
	// emptied by DrainTransitions. Unbounded on purpose: the producer is our own
	// state machine, bounded by the child's event rate, and the consumer is
	// monitorChild, which lives as long as the child and is woken on every
	// record. Capping it would mean dropping transitions, which is the very bug
	// this queue exists to fix.
	transitions []Transition
	// transitionCh is a capacity-1 wake for the consumer. It carries no data —
	// the queue does. This is what makes delivery prompt as well as loss-free:
	// draining only on bus frames would strand a turn's final streaming→idle
	// transition until the NEXT frame arrived, which for a child that has gone
	// quiet is never. It also cannot be a data channel: a bounded one would
	// either block the state machine or drop transitions, and the bus already
	// demonstrates the second failure mode (Publish drops on a full subscriber
	// buffer, so a status frame ON the bus would be lossy under exactly the
	// burst this fix is about).
	transitionCh chan struct{}
	idle         chan struct{}
	idleOnce     sync.Once
}

// Spawn validates the spec, starts the pi binary, and launches the supervise
// goroutine. The returned Child is immediately usable; wait on Ready() before
// sending commands if you need the supervise loop to be processing.
func Spawn(ctx context.Context, spec SpawnSpec) (*Child, error) {
	if !filepath.IsAbs(spec.Cwd) {
		return nil, fmt.Errorf("cwd must be absolute: %q", spec.Cwd)
	}
	// Only a subprocess runner (spec.Runner == nil) actually forks a real OS
	// process rooted at Cwd (newProcessRunner sets cmd.Dir = spec.Cwd below) —
	// that is the only case where Cwd must exist on THIS machine. An injected
	// Runner (the in-process fundi engine) never touches this filesystem for
	// Cwd; its own filesystem access, if any, goes through whichever executor
	// gets bound, which validates its own root independently. Stat-ing here
	// unconditionally is the same daemon-local-fs assumption already fixed in
	// Controller.Spawn — this is the second, previously-unreached instance of
	// it, since every spawn (fundi included) still flows through this Spawn.
	if spec.Runner == nil {
		if _, err := os.Stat(spec.Cwd); err != nil {
			return nil, fmt.Errorf("cwd: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	r := spec.Runner
	if r == nil {
		pr, perr := newProcessRunner(spec)
		if perr != nil {
			return nil, perr
		}
		r = pr
	}
	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		return nil, err
	}

	prov := spec.Provider
	if prov == nil {
		prov = PiProvider{}
	}
	// Each Child gets its own provider instance so a stateful translator
	// (ClaudeProvider) never shares accumulation state across children.
	prov = prov.Fresh()

	c := &Child{
		ID:           spec.ChildID,
		spec:         spec,
		runner:       r,
		stdin:        stdin,
		stdout:       stdout,
		stderr:       stderr,
		cmdCh:        make(chan []byte, 16),
		ready:        make(chan struct{}),
		done:         make(chan struct{}),
		processDone:  make(chan struct{}),
		bus:          bus.New[[]byte](bus.Options{}),
		ring:         ring.New(ring.Options{}),
		sm:           NewStateMachine(),
		provider:     prov,
		nativeSink:   spec.NativeSink,
		transitionCh: make(chan struct{}, 1),
		idle:         make(chan struct{}),
		abandonAfter: abandonTimeout,
	}

	if c.provider.Normalizes() {
		c.renderRing = ring.New(ring.Options{})
	}

	go c.supervise()
	return c, nil
}

// PID returns the operating system process ID of the child. Safe to call
// after Spawn returns.
func (c *Child) PID() int { return c.runner.PID() }

// ExitResult returns the shutdown result recorded after the child exits.
// Call only after Done() is closed; before that the fields are zero-valued.
func (c *Child) ExitResult() ShutdownResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exit
}

// Ready returns a channel that is closed once the supervise loop is running
// and processing commands. It does NOT guarantee pi has responded to anything.
func (c *Child) Ready() <-chan struct{} { return c.ready }

// Done returns a channel that is closed after the child process has exited and
// been reaped. Safe to select on at any time.
func (c *Child) Done() <-chan struct{} { return c.done }

// Bus returns the event bus that receives every stdout frame as it arrives.
func (c *Child) Bus() *bus.Bus[[]byte] { return c.bus }

// Ring returns the bounded ring buffer that stores recent stdout frames.
func (c *Child) Ring() *ring.Ring { return c.ring }

// Send queues a JSONL frame for delivery to pi's stdin. It returns immediately;
// the write happens in the supervise goroutine. Returns "backpressure" if the
// command channel is full, or "child shutting down" if the process has exited.
func (c *Child) Send(frame []byte) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("child shutting down")
	}
	select {
	case c.cmdCh <- frame:
		return nil
	default:
		return errors.New("backpressure")
	}
}

// Idle returns a channel that is closed once the kickstart get_state response
// has arrived and the state machine has transitioned out of spawning. This is
// a stronger signal than Ready(), which closes as soon as the write loop is
// processing (before pi has responded).
func (c *Child) Idle() <-chan struct{} { return c.idle }

// Status returns the current state-machine status. Safe to call concurrently.
func (c *Child) Status() protocol.Status {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	return c.sm.Current()
}

// PreShutdownStatus returns the status the child had before BeginShutdown
// transitioned it to shutting_down. Calling this is meaningless on a child
// for which BeginShutdown was never called — it returns the zero value of
// protocol.Status (""), which is not a valid status and never matches any
// real state. This exists solely so handleChildExit can record the child's
// real pre-exit status rather than "shutting_down" when the daemon is
// gracefully shutting down.
func (c *Child) PreShutdownStatus() protocol.Status {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	return c.preShutdownStatus
}

// recordTransition queues a status change for DrainTransitions and wakes the
// consumer. changed/prev are exactly what the StateMachine's On* methods
// return, so a no-op call (changed==false) is free and every caller can pass
// the result through without branching.
//
// Callers MUST hold metaMu, so that the transition is queued in the same
// critical section that made it: two goroutines transitioning concurrently can
// then never record their changes in the opposite order to the one the state
// machine applied them in.
func (c *Child) recordTransition(changed bool, prev protocol.Status) {
	if !changed {
		return
	}
	c.transitions = append(c.transitions, Transition{From: prev, To: c.sm.Current()})
	// The select/default is DEADLOCK-critical, not merely drop-avoiding. This
	// runs with metaMu held, and the only possible receiver's drain
	// (DrainTransitions) must take metaMu to empty the queue. A blocking send on
	// a full channel would therefore park readStdout while it holds the very
	// lock the consumer needs to make room: readStdout and monitorChild
	// deadlocked against each other. Never "improve" this into a blocking send,
	// and never widen the channel to make blocking unlikely — the queue carries
	// the data, the channel is only a doorbell.
	//
	// Dropping the token is free: a wake already pending covers this record too,
	// since the consumer drains the whole queue per wake. Nothing but the
	// consumer's select ever receives from the channel — a spurious wake with an
	// empty queue is a no-op, whereas swallowing a wake would strand a
	// transition.
	select {
	case c.transitionCh <- struct{}{}:
	default:
	}
}

// StatusChanged returns a channel that receives a value whenever one or more
// status transitions have been queued. Consumers must call DrainTransitions to
// read them; the channel carries no data and may fire spuriously (an empty
// drain is harmless).
func (c *Child) StatusChanged() <-chan struct{} { return c.transitionCh }

// DrainTransitions removes and returns every queued status transition, oldest
// first. Safe to call concurrently; returns nil when there is nothing queued.
//
// Every transition the child makes on its own goroutines is queued here, so a
// consumer that drains — rather than comparing Status() against a remembered
// value — cannot miss one. The single exception is BeginShutdown; see its doc.
func (c *Child) DrainTransitions() []Transition {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	if len(c.transitions) == 0 {
		return nil
	}
	// Hand the backing array to the caller and start a FRESH slice. `nil` here
	// is load-bearing, not tidiness: `c.transitions[:0]` would keep the same
	// backing array, so the next append would overwrite the very slots the
	// caller is still iterating — a data race the detector would only sometimes
	// catch, since it needs a record to land during the caller's loop. Dropping
	// the accumulated capacity is a side benefit, not the reason.
	out := c.transitions
	c.transitions = nil
	return out
}

// BeginShutdown transitions the state machine to shutting_down. It is called
// by the controller before invoking Shutdown() so that status-change events
// can be emitted before the graceful-shutdown sequence begins. Returns whether
// the transition occurred and the previous status. Safe to call concurrently.
//
// This transition is deliberately NOT queued for DrainTransitions. It is the
// one transition the controller makes itself rather than observes: it gets
// (changed, prev) back here and emits the status event synchronously, before
// starting the shutdown sequence, which is the ordering its callers depend on.
// Queueing it as well would deliver that one status change twice.
func (c *Child) BeginShutdown() (changed bool, prev protocol.Status) {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	changed, prev = c.sm.OnShutdownStart()
	if changed {
		c.preShutdownStatus = prev
	}
	return
}

// InSnapshot returns a defensive copy of all stdin frames captured since spawn.
// Safe to call at any time; typically called from handleChildExit after Done().
func (c *Child) InSnapshot() [][]byte {
	return c.in.snapshot()
}

// RingSnapshot returns a copy of all events currently in the ring buffer.
// Safe to call at any time; typically called from handleChildExit after Done().
func (c *Child) RingSnapshot() [][]byte {
	events := c.ring.Recent(ring.Query{})
	out := make([][]byte, len(events))
	for i, e := range events {
		out[i] = e.Bytes // ring.Recent already returns defensive copies
	}
	return out
}

// publishBus appends a bus frame to the render-ring (when the provider
// normalizes) and publishes it. The render-ring captures the exact
// pi-vocabulary stream the bus carries — assistant turns and synthesized user
// turns — so backfill can render claude children. Safe for concurrent use: the
// c.renderRing != nil read is unsynchronized but safe because renderRing is set
// once in Spawn before the readStdout and supervise goroutines start (write
// once, before publish), and ring.Ring's own mutex covers the concurrent
// Append from those two goroutines.
func (c *Child) publishBus(f []byte, ts int64) {
	if c.renderRing != nil {
		c.renderRing.Append(f, ts)
	}
	c.bus.Publish(f)
}

// RenderRingSnapshot returns a copy of all frames in the render-ring, or nil
// when the provider does not normalize (no render-ring).
func (c *Child) RenderRingSnapshot() [][]byte {
	if c.renderRing == nil {
		return nil
	}
	events := c.renderRing.Recent(ring.Query{})
	out := make([][]byte, len(events))
	for i, e := range events {
		out[i] = e.Bytes
	}
	return out
}

// RenderRecent returns render-ring events matching q, or nil when there is no
// render-ring.
func (c *Child) RenderRecent(q ring.Query) []ring.Event {
	if c.renderRing == nil {
		return nil
	}
	return c.renderRing.Recent(q)
}

// RenderStats returns the render-ring's event count and oldest timestamp, or
// (0,0) when there is no render-ring.
func (c *Child) RenderStats() (events int, oldestTimestamp int64) {
	if c.renderRing == nil {
		return 0, 0
	}
	n, _, oldest := c.renderRing.Stats()
	return n, oldest
}

// Normalizes reports whether this child's provider translates stdout into a
// distinct bus stream (claude). When true, the render-ring is the renderable
// source; when false, the raw ring is already renderable.
func (c *Child) Normalizes() bool { return c.provider.Normalizes() }

// busFramesNative translates one raw stdout line into native events via the
// provider, when it supports native translation (claudeProvider).
func (c *Child) busFramesNative(line []byte, ts int64) []*rafikiv1.Event {
	type nativeTranslator interface {
		BusFramesNative([]byte, int64) []*rafikiv1.Event
	}
	if nt, ok := c.provider.(nativeTranslator); ok {
		return nt.BusFramesNative(line, ts)
	}
	return nil
}

// StderrSnapshot returns a copy of buffered stderr bytes. Safe to call at any
// time: readStderr's writes and this read share errMu, because Done() being
// closed no longer proves readStderr has finished (see abandon).
func (c *Child) StderrSnapshot() []byte {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	b := make([]byte, c.errBuf.Len())
	copy(b, c.errBuf.Bytes())
	return b
}

// NotifyExtensionUIResponse updates the state machine when the controller
// forwards an extension_ui_response to the child. If this was the last
// pending dialog, the SM transitions from blocked_ui back to the prior state.
// The status change is observed by monitorChild on the next bus event.
func (c *Child) NotifyExtensionUIResponse(id string) {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	c.recordTransition(c.sm.OnExtensionUIResponse(id))
}

// Metadata returns the most recently sniffed session/model metadata.
// Safe to call concurrently.
func (c *Child) Metadata() SnifferMetadata {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	return c.meta
}

// supervise is the goroutine that owns the child's lifecycle. It launches the
// stdout and stderr reader goroutines, then drives the stdin write loop until
// the process exits. defer close(c.done) makes Done() observable to callers.
func (c *Child) supervise() {
	defer c.closeDone()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.readStdout() }()
	go func() { defer wg.Done(); c.readStderr() }()

	// Signal that the write loop is running, then queue the provider's bootstrap
	// frame (if any). cmdCh has capacity 16 so this never blocks.
	close(c.ready)
	if boot := c.provider.BootstrapFrame(); boot != nil {
		c.cmdCh <- boot
	}

	// Process-up readiness: a ReadyOnSpawn provider (claude) emits nothing on
	// stdout until it is prompted, so there is no readiness signal to wait for —
	// the child is ready for input the instant it launches (stdin is buffered).
	// Fire spawning→idle now so subagent_send is unblocked; the first turn's
	// metadata (session id/model) is sniffed later from its stdout. Without this
	// such a child would sit in spawning until activateLiveChild's idle wait timed
	// out (stalled), and the store status would never advance.
	if c.provider.ReadyOnSpawn() {
		c.metaMu.Lock()
		c.recordTransition(c.sm.OnFirstResponse())
		c.metaMu.Unlock()
		c.idleOnce.Do(func() { close(c.idle) })
	}

	for {
		select {
		case frame := <-c.cmdCh:
			out := c.provider.EncodeOutbound(frame)
			if out == nil {
				// Provider dropped a frame unsupported by this protocol.
				// The sender already got a success response (Send only
				// checks channel acceptance), so this is the only
				// visibility into a silently-lost command.
				slog.Warn("frame dropped by provider (unsupported)", "child", c.ID,
					"frame_type", frameType(frame))
				continue
			}
			if _, err := c.stdin.Write(out); err != nil {
				slog.Warn("stdin write failed", "child", c.ID, "error", err)
				goto cleanup
			}
			if _, err := c.stdin.Write([]byte{'\n'}); err != nil {
				slog.Warn("stdin newline write failed", "child", c.ID, "error", err)
				goto cleanup
			}
			// Publish any provider-synthesized echo (e.g. the user message for a
			// claude child, which never echoes the prompt on its own stdout) so the
			// user's turn reaches the bus. Done after the stdin write but before the
			// child can respond, so it lands ahead of the assistant frames.
			echoTS := time.Now().UnixMilli()
			for _, f := range c.provider.OutboundEcho(frame, echoTS) {
				c.publishBus(f, echoTS)
			}
			c.in.append(frame) // capture the original (normalized) frame for log dumps
		case <-c.processDone:
			// Process exited; drain nothing, let cleanup wait for readers.
			goto cleanup
		}
	}
cleanup:
	wg.Wait()

	// Release the daemon-side ends of ALL THREE of the child's streams. Until
	// this existed, NOTHING in the daemon ever closed c.stdout or c.stderr:
	// they were reclaimed only by os.File finalizers, and FD exhaustion does not
	// trigger a GC — so a daemon churning children can hit EMFILE with a
	// perfectly small heap. In-process children make that far more than
	// theoretical, since self-exit (a failed Build, a frontend scan error, a
	// contained panic) is their common case rather than an exception.
	//
	// stdin belongs here for exactly the same reason, and the omission left the
	// same leak on the most common path of all. Child.Shutdown closes it, but
	// handleChildExit does NOT call Shutdown — its only callers are
	// Controller.Kill and ShutdownAllChildren — so a child that ends on its own
	// left its stdin write end to a finalizer. This block is the symmetric
	// home: supervise is the only writer to c.stdin and has already left the
	// write loop above, whichever way it got here.
	//
	// Safe here and nowhere earlier: wg.Wait() above means readStdout and
	// readStderr have both returned, so every read is complete, and readStdout
	// only returns after runner.Wait() has reaped the child.
	//
	// Composes with both of Shutdown's own closes (its unconditional stdin
	// close, and abandon's stdout/stderr close on the leaked path) because
	// closeStream treats os.ErrClosed as success — whichever runs second is a
	// no-op rather than a spurious warning.
	closeStream(c.ID, "stdin", c.stdin)
	closeStream(c.ID, "stdout", c.stdout)
	closeStream(c.ID, "stderr", c.stderr)
}

// closeDone closes c.done at most once. There are two closers — supervise on
// the ordinary path, and Shutdown's abandon path — and they are not mutually
// exclusive: an abandoned child's supervise goroutine can still complete
// later, if the syscall its runner was stuck in finally returns. A plain
// `close` in both places would panic the whole daemon at that moment.
func (c *Child) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// closeStream closes one of the child's stream handles, logging any failure.
// An already-closed handle is not a failure: Shutdown closes stdin on its own
// path, and a runner (inproc.Runner.Kill) may have closed a read end to force
// a wedged child down.
func closeStream(childID, name string, s io.Closer) {
	if s == nil {
		return
	}
	if err := s.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		slog.Warn("close child stream", "child", childID, "stream", name, "error", err)
	}
}

// readStdout reads JSONL frames from the child's stdout, publishes them to the
// bus and ring, then reaps the process. It signals processDone when done so
// the supervise loop can exit cleanly.
func (c *Child) readStdout() {
	r := protocol.NewFrameReader(c.stdout, protocol.MaxFrameBytes)
	for {
		line, err := r.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("stdout read error", "child", c.ID, "error", err)
			break
		}
		ts := time.Now().UnixMilli()
		// The ring keeps the RAW child frame (so subagent_view raw stays
		// forensically real); the bus carries the provider's normalized pi
		// AgentSessionEvent frames. For pi these are identical (identity
		// provider); for claude the provider translates raw → pi vocabulary.
		//
		// message_update frames are excluded from the ring on purpose. At
		// streaming volume a single turn emits hundreds of them, each carrying
		// the whole accumulated message, which would evict the entire
		// conversation from the bounded ring — and attach's primeHistory skips
		// message_update anyway, so it would prime blank scrollback from them.
		// Nothing is lost: the message_end that follows carries everything the
		// deltas built up. Only delta timing is dropped. The bus is unaffected:
		// BusFrames/publishBus below still see and publish every frame, so live
		// subscribers keep getting every delta.
		if !isMessageUpdate(line) {
			c.ring.Append(line, ts)
		}
		for _, f := range c.provider.BusFrames(line, ts) {
			c.publishBus(f, ts)
		}
		if c.nativeSink != nil {
			for _, ev := range c.busFramesNative(line, ts) {
				ev.ChildId = c.ID
				c.nativeSink(ev)
			}
		}
		c.handleFrame(line)
	}

	// Reap the process and record its exit status.
	code, sig := c.runner.Wait()
	c.mu.Lock()
	if c.abandoned {
		// Shutdown gave up on this reap and already published an outcome; the
		// daemon has moved on (the store record is written, the log dump
		// taken, ctrl_child_exited delivered). Overwriting c.exit now would
		// silently contradict what everyone was told, so keep the record and
		// just note that the straggler did eventually land.
		c.mu.Unlock()
		slog.Info("abandoned child was reaped after all; keeping the abandoned exit record",
			"child", c.ID, "late_exit_code", code, "late_signal", sig)
	} else {
		c.exit.ExitCode = code
		c.exit.Signal = sig
		c.closed = true
		c.mu.Unlock()
	}

	close(c.processDone)
}

// isMessageUpdate reports whether line is a message_update frame. It runs on
// every frame from every child, so it decodes only the type field via a
// minimal struct — the same cheap-extraction approach cmd/rafikid's
// eventPassesFilter uses for subscriber filtering — rather than fully
// unmarshaling the (potentially multi-KB) message payload. An unparseable
// frame returns false so it is still appended to the ring: a parse failure is
// not grounds for silently dropping a child's output.
func isMessageUpdate(line []byte) bool {
	var hdr struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &hdr); err != nil {
		return false
	}
	return hdr.Type == "message_update"
}

// frameType extracts the "type" field from a JSON-encoded outbound frame for
// logging.  Returns "?" on parse failure; the caller must handle the nil-output
// path, not this helper.
func frameType(line []byte) string {
	var hdr struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &hdr); err != nil {
		return "?"
	}
	if hdr.Type == "" {
		return "?"
	}
	return hdr.Type
}

// handleFrame routes one stdout line through the child's ProtocolProvider and
// applies the normalized result to the state machine + sniffed metadata. Called
// exclusively from the readStdout goroutine.
func (c *Child) handleFrame(line []byte) {
	res := c.provider.Parse(line)

	c.metaMu.Lock()

	if res.FirstResponse {
		c.recordTransition(c.sm.OnFirstResponse())
	}

	if res.HasMeta {
		md := res.Meta
		if md.SessionID != "" {
			c.meta.SessionID = md.SessionID
		}
		if md.SessionFile != "" {
			c.meta.SessionFile = md.SessionFile
		}
		if md.SessionName != "" {
			c.meta.SessionName = md.SessionName
		}
		if md.Model != "" {
			c.meta.Model = md.Model
		}
		if len(md.SlashCommands) > 0 {
			c.meta.SlashCommands = md.SlashCommands
		}
	}

	for _, e := range res.Events {
		switch e.Type {
		case "auto_retry_start":
			// Informational only; OnAutoRetryStart makes no transition.
			c.sm.OnAutoRetryStart(e.RetryError)
		case "extension_ui_request":
			c.recordTransition(c.sm.OnPiEvent(e.Type, e.UI))
		default:
			c.recordTransition(c.sm.OnPiEvent(e.Type, nil))
		}
	}

	c.metaMu.Unlock()

	if res.FirstResponse {
		c.idleOnce.Do(func() { close(c.idle) })
	}
}

// readStderr drains stderr into a bounded in-memory buffer. Oldest bytes are
// dropped when the buffer would exceed 4 MiB.
func (c *Child) readStderr() {
	br := bufio.NewReader(c.stderr)
	buf := make([]byte, 4096)
	const maxErr = 4 << 20
	for {
		n, err := br.Read(buf)
		if n > 0 {
			c.errMu.Lock()
			if c.errBuf.Len()+n > maxErr {
				// Drop oldest to stay within the cap.
				trim := c.errBuf.Len() + n - maxErr
				if trim >= c.errBuf.Len() {
					c.errBuf.Reset()
				} else {
					c.errBuf.Next(trim)
				}
			}
			c.errBuf.Write(buf[:n])
			c.errMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Shutdown attempts a graceful shutdown:
//  1. Close stdin. Wait up to shutdownTimeout.
//  2. SIGTERM. Wait up to killTimeout.
//  3. SIGKILL. Wait up to abandonTimeout, then abandon (see abandon).
//
// Escalated is true if SIGTERM (or SIGKILL) was required; Abandoned is true if
// rung 3 expired without a reap.
func (c *Child) Shutdown(shutdownTimeout, killTimeout time.Duration) (ShutdownResult, error) {
	start := time.Now()

	c.mu.Lock()
	alreadyClosed := c.closed
	c.mu.Unlock()

	// Close stdin unconditionally, INCLUDING on the already-exited path. The
	// early return below used to skip it, which left the daemon-side write end
	// of an exited child's stdin open until an os.File finalizer happened to
	// run — and FD exhaustion does not trigger a GC, so that is a real EMFILE
	// path for a daemon churning children, not a tidiness point. Closing the
	// write end of a pipe whose reader is gone is harmless.
	closeStream(c.ID, "stdin", c.stdin)

	if alreadyClosed {
		// Process already exited; copy the stored result.
		c.mu.Lock()
		res := c.exit
		c.mu.Unlock()
		res.Duration = time.Since(start)
		return res, nil
	}

	// escalated is local state; only readStdout writes to c.exit (under c.mu).
	// We read c.exit once below under the lock, then set Escalated/Duration on
	// the local copy — no concurrent writes to shared state.
	escalated := false
	select {
	case <-c.done:
	case <-time.After(shutdownTimeout):
		// stdin close didn't cause a timely exit; escalate to SIGTERM.
		escalated = true
		if terr := c.runner.Terminate(); terr != nil {
			slog.Warn("terminate runner", "child", c.ID, "error", terr)
		}
		select {
		case <-c.done:
		case <-time.After(killTimeout):
			if kerr := c.runner.Kill(); kerr != nil {
				slog.Warn("kill runner", "child", c.ID, "error", kerr)
			}
			// Kill() is the last thing we can do TO the child; it is not proof
			// the child stopped. Bound the wait rather than hanging the caller
			// forever on a reap that may never come.
			select {
			case <-c.done:
			case <-time.After(c.abandonAfter):
				c.abandon() // records Abandoned on c.exit, read back below
			}
		}
	}

	c.mu.Lock()
	res := c.exit
	c.mu.Unlock()
	res.Escalated = escalated
	res.Duration = time.Since(start)
	return res, nil
}

// abandon gives up on a reap that is not coming and puts the child into a state
// where the still-running execution context cannot do damage when the syscall
// it is stuck in finally returns. Called only from Shutdown, only after Kill()
// has already run, and only once per child (Shutdown's already-exited early
// return catches every subsequent call).
//
// Leaking a goroutine is acceptable ONLY because of what this function
// guarantees first:
//
//  1. Everything the child could write to is closed. Kill() has already closed
//     the read ends of both pipes (inproc.Runner.Kill), which is what makes a
//     late Frontend.Emit fail with a broken pipe rather than succeed into a
//     structure the daemon has moved on from; Shutdown closed the daemon-side
//     stdin write end at its top. The two remaining daemon-side handles are
//     c.stdout and c.stderr, normally closed by supervise's cleanup block —
//     which is exactly what a leaked supervise (parked in wg.Wait()) will never
//     reach. Close them here. closeStream tolerates the already-closed case,
//     so supervise's own close is still correct if it ever gets there.
//  2. The context is cancelled, so no NEW work can start after the syscall
//     returns: Kill() calls Terminate() first, which cancels it.
//  3. The child is recorded as exited before this returns, so the daemon stops
//     treating it as live. closed=true makes Send reject frames, the exit
//     result is published as the forced-stop shape a reaped kill would have
//     produced, and closing done releases monitorChild into handleChildExit —
//     which is also what unblocks ShutdownAllChildren's post-Shutdown wait for
//     cm.Remove. abandoned=true makes the record final against a late reap
//     (see readStdout).
//  4. Nothing waits on the leaked goroutine again. done is closed here rather
//     than by supervise, so no future Shutdown, Done() select, or
//     handleChildExit blocks on it.
func (c *Child) abandon() {
	closeStream(c.ID, "stdout", c.stdout)
	closeStream(c.ID, "stderr", c.stderr)

	c.mu.Lock()
	c.abandoned = true
	c.closed = true
	// Report the same shape a reaped forced stop reports — exit code 0 with
	// signal "killed", the contract processRunner.Wait and inproc.Runner.Wait
	// both honour — so the wire answer to "I killed this child" does not change
	// depending on whether the reap happened to land. ShutdownResult.Abandoned
	// is what carries the difference.
	c.exit.ExitCode = 0
	c.exit.Signal = "killed"
	// Stored on the record itself, not just on the ShutdownResult this call
	// returns, so ExitResult() (what handleChildExit persists and what a second
	// Shutdown's already-exited early return copies) reports it too.
	c.exit.Abandoned = true
	c.mu.Unlock()

	c.closeDone()

	// Error level, with the child id: a goroutine leaked inside a long-lived
	// daemon is something an operator needs to see, not a debug detail. If the
	// child was an in-process agent, this leaks its engine goroutine (and
	// whatever it is blocked in) for the daemon's remaining lifetime.
	slog.Error("child never reaped after kill; abandoning the wait and leaking its execution context",
		"child", c.ID, "waited", c.abandonAfter)
}

// Interrupt sends SIGINT to the child's process group. For a claude child this
// makes claude flush a "[Request interrupted by user]" frame plus a
// result:error_during_execution, persist the (possibly partial) turn to its
// session store, and exit. It does NOT wait for exit — the caller observes the
// transition to StatusExited. A no-op if the process has already exited.
func (c *Child) Interrupt() error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil
	}
	return c.runner.Interrupt()
}
