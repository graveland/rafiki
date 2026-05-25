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

	"graveland.dev/pi-controller/internal/bus"
	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/ring"
)

// SpawnSpec describes how to launch a pi child process.
type SpawnSpec struct {
	ChildID   string
	Cwd       string
	PiBinary  string
	Argv      []string // full argv excluding PiBinary itself
	ExtraArgs []string // appended after Argv; useful in tests
	Env       []string // additions appended to os.Environ(); nil means inherit
}

// ShutdownResult records the outcome of a graceful-shutdown sequence.
type ShutdownResult struct {
	ExitCode  int
	Signal    string
	Escalated bool // true if SIGTERM or SIGKILL was required
	Duration  time.Duration
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

	bus  *bus.Bus[[]byte]
	ring *ring.Ring

	errBuf bytes.Buffer // written by readStderr only; any future reader must hold c.mu

	mu     sync.Mutex
	closed bool // set by readStdout after cmd.Process.Wait() returns
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
		cmd.Env = append(os.Environ(), spec.Env...)
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
	}

	go c.supervise()
	return c, nil
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

// supervise is the goroutine that owns the child's lifecycle. It launches the
// stdout and stderr reader goroutines, then drives the stdin write loop until
// the process exits. defer close(c.done) makes Done() observable to callers.
func (c *Child) supervise() {
	defer close(c.done)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.readStdout() }()
	go func() { defer wg.Done(); c.readStderr() }()

	// Signal that the supervise loop is now processing. State-machine wiring
	// (Task 12) will refine this to close only after the first pi response.
	close(c.ready)

	for {
		select {
		case frame := <-c.cmdCh:
			if _, err := c.stdin.Write(frame); err != nil {
				slog.Warn("stdin write failed", "child", c.ID, "error", err)
				goto cleanup
			}
			if _, err := c.stdin.Write([]byte{'\n'}); err != nil {
				slog.Warn("stdin newline write failed", "child", c.ID, "error", err)
				goto cleanup
			}
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
	r := protocol.NewFrameReader(c.stdout, 16<<20)
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
		c.ring.Append(line, ts)
		c.bus.Publish(line)
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
