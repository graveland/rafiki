package child

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"git.graveland.dev/brent/fundi/internal/bus"
	"git.graveland.dev/brent/fundi/internal/ring"
	"git.graveland.dev/brent/fundi/protocol"
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
}

// ShutdownResult records the outcome of a graceful-shutdown sequence.
type ShutdownResult struct {
	ExitCode  int
	Signal    string
	Escalated bool // true if SIGTERM or SIGKILL was required
	Duration  time.Duration
}

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
	ID   string
	spec SpawnSpec
	cmd  *exec.Cmd

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	cmdCh       chan []byte
	ready       chan struct{} // closed once supervise loop is processing
	done        chan struct{} // closed when child has exited and been reaped
	processDone chan struct{} // internal: closed by readStdout after Wait()

	exit ShutdownResult

	bus        *bus.Bus[[]byte]
	ring       *ring.Ring
	renderRing *ring.Ring // bus-frame capture for normalizing providers (claude); nil otherwise
	in         inBuffer   // bounded stdin frame capture for log dumps

	errBuf bytes.Buffer // written by readStderr only; safe to read after Done() is closed

	mu     sync.Mutex
	closed bool // set by readStdout after cmd.Process.Wait() returns

	sm       *StateMachine
	provider ProtocolProvider
	// metaMu protects meta and sm to allow Status()/Metadata() concurrent reads.
	metaMu   sync.Mutex
	meta     SnifferMetadata
	idle     chan struct{}
	idleOnce sync.Once
}

// Spawn validates the spec, starts the pi binary, and launches the supervise
// goroutine. The returned Child is immediately usable; wait on Ready() before
// sending commands if you need the supervise loop to be processing.
func Spawn(ctx context.Context, spec SpawnSpec) (*Child, error) {
	if spec.PiBinary == "" {
		return nil, errors.New("pi binary path required")
	}
	if !filepath.IsAbs(spec.Cwd) {
		return nil, fmt.Errorf("cwd must be absolute: %q", spec.Cwd)
	}
	if _, err := os.Stat(spec.Cwd); err != nil {
		return nil, fmt.Errorf("cwd: %w", err)
	}

	argv := append([]string{}, spec.Argv...)
	argv = append(argv, spec.ExtraArgs...)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	cmd := exec.Command(spec.PiBinary, argv...)
	cmd.Dir = spec.Cwd
	// Put the child in its own process group so that any subprocesses it spawns
	// can be killed as a group during shutdown (prevents orphan children from
	// keeping pipe write ends open and blocking our readers).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(spec.Env) > 0 {
		if spec.EnvOverride {
			cmd.Env = append([]string{}, spec.Env...) // override: use only the specified env
		} else {
			cmd.Env = append(os.Environ(), spec.Env...) // merge: inherit parent env plus additions
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	prov := spec.Provider
	if prov == nil {
		prov = PiProvider{}
	}
	// Each Child gets its own provider instance so a stateful translator
	// (ClaudeProvider) never shares accumulation state across children.
	prov = prov.Fresh()

	c := &Child{
		ID:          spec.ChildID,
		spec:        spec,
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		cmdCh:       make(chan []byte, 16),
		ready:       make(chan struct{}),
		done:        make(chan struct{}),
		processDone: make(chan struct{}),
		bus:         bus.New[[]byte](bus.Options{}),
		ring:        ring.New(ring.Options{}),
		sm:          NewStateMachine(),
		provider:    prov,
		idle:        make(chan struct{}),
	}

	if c.provider.Normalizes() {
		c.renderRing = ring.New(ring.Options{})
	}

	go c.supervise()
	return c, nil
}

// PID returns the operating system process ID of the child. Safe to call
// after Spawn returns.
func (c *Child) PID() int { return c.cmd.Process.Pid }

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

// BeginShutdown transitions the state machine to shutting_down. It is called
// by the controller before invoking Shutdown() so that status-change events
// can be emitted before the graceful-shutdown sequence begins. Returns whether
// the transition occurred and the previous status. Safe to call concurrently.
func (c *Child) BeginShutdown() (changed bool, prev protocol.Status) {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	return c.sm.OnShutdownStart()
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

// StderrSnapshot returns a copy of buffered stderr bytes.
// Must only be called after Done() is closed; otherwise concurrent
// readStderr writes may race.
func (c *Child) StderrSnapshot() []byte {
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
	c.sm.OnExtensionUIResponse(id)
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
	defer close(c.done)

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
		c.sm.OnFirstResponse()
		c.metaMu.Unlock()
		c.idleOnce.Do(func() { close(c.idle) })
	}

	for {
		select {
		case frame := <-c.cmdCh:
			out := c.provider.EncodeOutbound(frame)
			if out == nil {
				// Provider dropped a frame unsupported by this protocol.
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
		c.ring.Append(line, ts)
		for _, f := range c.provider.BusFrames(line, ts) {
			c.publishBus(f, ts)
		}
		c.handleFrame(line)
	}

	// Reap the process and record its exit status.
	state, err := c.cmd.Process.Wait()
	c.mu.Lock()
	if err != nil {
		c.exit.ExitCode = -1
	} else {
		if state.ExitCode() >= 0 {
			c.exit.ExitCode = state.ExitCode()
		}
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			c.exit.Signal = ws.Signal().String()
		}
	}
	c.closed = true
	c.mu.Unlock()

	close(c.processDone)
}

// handleFrame routes one stdout line through the child's ProtocolProvider and
// applies the normalized result to the state machine + sniffed metadata. Called
// exclusively from the readStdout goroutine.
func (c *Child) handleFrame(line []byte) {
	res := c.provider.Parse(line)

	c.metaMu.Lock()

	if res.FirstResponse {
		c.sm.OnFirstResponse()
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
			c.sm.OnAutoRetryStart(e.RetryError)
		case "extension_ui_request":
			c.sm.OnPiEvent(e.Type, e.UI)
		default:
			c.sm.OnPiEvent(e.Type, nil)
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
		}
		if err != nil {
			return
		}
	}
}

// Shutdown attempts a graceful shutdown:
//  1. Close stdin. Wait up to shutdownTimeout.
//  2. SIGTERM. Wait up to killTimeout.
//  3. SIGKILL. Reap.
//
// Escalated is true if SIGTERM (or SIGKILL) was required.
func (c *Child) Shutdown(shutdownTimeout, killTimeout time.Duration) (ShutdownResult, error) {
	start := time.Now()

	c.mu.Lock()
	alreadyClosed := c.closed
	c.mu.Unlock()

	if alreadyClosed {
		// Process already exited; copy the stored result.
		c.mu.Lock()
		res := c.exit
		c.mu.Unlock()
		res.Duration = time.Since(start)
		return res, nil
	}

	_ = c.stdin.Close()

	// escalated is local state; only readStdout writes to c.exit (under c.mu).
	// We read c.exit once below under the lock, then set Escalated/Duration on
	// the local copy — no concurrent writes to shared state.
	escalated := false
	select {
	case <-c.done:
	case <-time.After(shutdownTimeout):
		// stdin close didn't cause a timely exit; escalate to SIGTERM.
		// Kill the entire process group so that any subprocesses the child
		// spawned also receive the signal.
		escalated = true
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(killTimeout):
			// SIGTERM didn't work; force-kill the process group.
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			<-c.done
		}
	}

	c.mu.Lock()
	res := c.exit
	c.mu.Unlock()
	res.Escalated = escalated
	res.Duration = time.Since(start)
	return res, nil
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
	if c.cmd == nil || c.cmd.Process == nil {
		return fmt.Errorf("interrupt: no process handle")
	}
	// Signal the whole process group (negative PID), matching Shutdown, so any
	// subprocess the child spawned is interrupted too. A process that exited
	// between the closed check and here yields ESRCH — treat that as the no-op
	// the caller asked for, not an error.
	if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
