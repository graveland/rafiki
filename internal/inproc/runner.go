// Package inproc runs a fundi agent as a goroutine inside the daemon rather
// than as a subprocess. It implements child.Runner over a pair of real OS
// pipes (os.Pipe), so the daemon's frame loops are identical for both
// execution models — including buffering: an OS pipe's kernel buffer matches
// what a subprocess's stdio actually gives the daemon, which an unbuffered
// io.Pipe does not (see Start's doc comment).
//
// This package exists because internal/agent imports internal/child: an
// in-process runner cannot live in internal/child without an import cycle.
// Nothing imports this package except cmd/fundid.
package inproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"git.graveland.dev/brent/fundi/internal/agent"
	"git.graveland.dev/brent/fundi/internal/child"
)

// BuildFunc constructs the engine. Defaults to agent.BuildRuntime; tests
// substitute it to inject failures a real builder cannot produce on demand.
type BuildFunc func(ctx context.Context, fe *agent.Frontend, ro agent.RuntimeOptions) (*agent.Engine, func(), error)

// Options configures a Runner.
type Options struct {
	ChildID string
	Runtime agent.RuntimeOptions
	// Parent is the daemon's context. The runner derives a cancellable child
	// from it, so cancelling Parent stops every in-process child at once.
	Parent context.Context
	// Build defaults to agent.BuildRuntime when nil.
	Build BuildFunc
}

// Runner drives an agent.Engine in a goroutine. It satisfies child.Runner.
type Runner struct {
	opts Options
	done chan struct{}

	mu       sync.Mutex
	cancel   context.CancelFunc
	exitCode int
	eng      *agent.Engine
	stdinR   *os.File
	stdoutR  *os.File
}

// New returns a Runner for opts. Nothing runs until Start is called.
func New(opts Options) *Runner {
	if opts.Build == nil {
		opts.Build = agent.BuildRuntime
	}
	if opts.Parent == nil {
		opts.Parent = context.Background()
	}
	return &Runner{opts: opts, done: make(chan struct{})}
}

// Compile-time proof that Runner satisfies the seam it exists for.
var _ child.Runner = (*Runner)(nil)

// Start wires two pipes and launches the engine goroutine. The daemon writes
// frames to the returned stdin and reads them from the returned stdout.
//
// These are real OS pipes (os.Pipe), not io.Pipe: agent.Frontend.Emit issues
// two separate Write calls per frame (the JSON bytes, then a bare "\n"), both
// under the same mutex. Over an unbuffered io.Pipe, any Write that the reader
// doesn't immediately and fully consume blocks — and because both Writes hold
// Emit's mutex, one blocked Write freezes ALL further emission for this child,
// with no error and no timeout, not just the one frame in flight. That
// zero-slack lockstep between writer and reader pace is itself a divergence
// from the subprocess model this package must be a drop-in for: a real
// subprocess's stdio gives the daemon a kernel-buffered pipe (tens of KB), so
// a slow or momentarily-stalled reader never blocks the child's writer. An
// os.Pipe reproduces that same slack.
//
// Kill retains the read end of this stdout pipe (stdoutR) for exactly the
// mirror-image reason: even a kernel buffer is finite, and if the daemon ever
// stops reading stdout for good (see Kill's doc comment), the engine's next
// Write blocks forever holding Emit's mutex. Closing stdoutR is what
// guarantees Kill is always effective.
func (r *Runner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inproc: stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		if cerr := stdinR.Close(); cerr != nil {
			slog.Warn("inproc: close stdin reader after failed stdout pipe", "child", r.opts.ChildID, "error", cerr)
		}
		if cerr := stdinW.Close(); cerr != nil {
			slog.Warn("inproc: close stdin writer after failed stdout pipe", "child", r.opts.ChildID, "error", cerr)
		}
		return nil, nil, nil, fmt.Errorf("inproc: stdout pipe: %w", err)
	}

	ctx, cancel := context.WithCancel(r.opts.Parent)
	r.mu.Lock()
	r.cancel = cancel
	r.stdinR = stdinR
	r.stdoutR = stdoutR
	r.mu.Unlock()

	go r.run(ctx, stdinR, stdoutW)

	// An in-process agent has no separate diagnostic stream: its logs go to the
	// daemon's logger tagged with the child id. Return a reader already at EOF
	// so readStderr completes immediately rather than being handed a nil.
	return stdinW, stdoutR, io.NopCloser(strings.NewReader("")), nil
}

// run owns the engine's whole lifetime. Its defers, in REGISTRATION order
// (they execute in the reverse of this order):
//
//  1. close(r.done) — registered first, so it runs LAST, after every other
//     defer below has completed. This is what makes "r.done is closed" mean
//     "shutdown() and Engine.Close() have already finished" to Wait's caller.
//  2. the recover-and-close-stdout defer — runs after stdinR is closed but
//     before close(r.done). It contains panic recovery (so a panicking Build
//     or fe.Run doesn't crash the daemon) and unconditionally closes stdoutW,
//     which is what turns a contained panic into an ordinary EOF for the
//     daemon's reader.
//  3. defer stdinR.Close() — registered only once Build has run (it is a
//     plain defer statement below, not conditional on success), so it always
//     fires before #2 sees stdoutW close.
//  4. defer shutdown() — registered only after Build succeeds, so it never
//     runs on a build-error exit. Runs before #3's stdinR.Close().
//
// Net effect: shutdown() and Engine.Close() (called explicitly, near the end
// of this function body) both complete before stdout closes, which is what
// makes the EOF the daemon sees on stdout mean "this child is finished".
func (r *Runner) run(ctx context.Context, stdinR *os.File, stdoutW *os.File) {
	defer close(r.done)
	defer func() {
		if v := recover(); v != nil {
			slog.Error("inproc: agent panicked",
				"child", r.opts.ChildID, "panic", v, "stack", string(debug.Stack()))
			r.setExit(2)
			// The engine's turn worker (if Build got far enough to create one)
			// may still be running; cancelling ctx unblocks anything selecting
			// on it. r.eng is readable here (under mu), but calling Close() on
			// it is NOT safe: Close's contract requires Wait() to have already
			// returned, and Wait() can block indefinitely on a genuinely wedged
			// turn — exactly what we cannot risk doing from inside panic
			// recovery, whose whole job is to return promptly. Left as an
			// accepted, bounded leak (one goroutine) on a path that should
			// never execute in practice.
			r.mu.Lock()
			cancel := r.cancel
			r.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		if err := stdoutW.Close(); err != nil {
			slog.Warn("inproc: close stdout", "child", r.opts.ChildID, "error", err)
		}
	}()
	defer func() {
		if err := stdinR.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			slog.Warn("inproc: close stdin reader", "child", r.opts.ChildID, "error", err)
		}
	}()

	fe := agent.NewFrontend(stdinR, stdoutW, nil)
	eng, shutdown, err := r.opts.Build(ctx, fe, r.opts.Runtime)
	if err != nil {
		slog.Error("inproc: build engine", "child", r.opts.ChildID, "error", err)
		r.setExit(1)
		return
	}
	r.mu.Lock()
	r.eng = eng
	r.mu.Unlock()
	defer shutdown()

	// Frontend.Run returns only on stdin EOF or a scan error, so no further
	// HandlePrompt/HandleSteer/HandleAbort can arrive afterwards — which is what
	// makes Wait-then-Close race-free. Same ordering as cmd/fundid/agent.go.
	if runErr := fe.Run(); runErr != nil {
		slog.Error("inproc: frontend run", "child", r.opts.ChildID, "error", runErr)
		r.setExit(1)
	}
	eng.Wait()
	eng.Close()
}

func (r *Runner) setExit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exitCode = code
}

// Wait blocks until the engine goroutine finishes. An in-process runner is
// never signalled, so the signal string is always empty.
func (r *Runner) Wait() (int, string) {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode, ""
}

// PID reports 0: there is no process. Callers persisting a PID must treat 0 as
// "no process to signal" — loadOrphans already does (`if rec.PID > 0`).
func (r *Runner) PID() int { return 0 }

// Terminate cancels the engine's context, aborting any turn in flight. The
// daemon calls this only after closing stdin failed to end the child in time.
func (r *Runner) Terminate() error {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Kill cancels the context and closes the read end of both pipes, so that
// Frontend.Run returns even if it is blocked on a read, AND — the case
// Terminate alone cannot handle — an engine goroutine blocked inside a stdout
// Write (agent.Frontend.Emit holds its mutex across that Write; see Start's
// doc comment) unblocks too: closing stdoutR makes that Write fail with a
// broken-pipe error instead of hanging forever. Without this, a daemon that
// stops reading a child's stdout (e.g. after a frame-too-large error) can wedge
// that child's run() goroutine permanently, and Kill would no longer be the
// guaranteed-effective escalation the daemon's shutdown ladder depends on.
//
// Both closes tolerate an already-closed file (a second Kill call, or a race
// with run()'s own stdinR close on ordinary EOF) rather than reporting it as
// an error.
func (r *Runner) Kill() error {
	if err := r.Terminate(); err != nil {
		return err
	}
	r.mu.Lock()
	stdinR := r.stdinR
	stdoutR := r.stdoutR
	r.mu.Unlock()

	var errs []error
	if stdinR != nil {
		if err := stdinR.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, fmt.Errorf("close stdin reader: %w", err))
		}
	}
	if stdoutR != nil {
		if err := stdoutR.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, fmt.Errorf("close stdout reader: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Interrupt aborts the current turn without stopping the runner — the
// in-process equivalent of SIGINT to a subprocess.
func (r *Runner) Interrupt() error {
	r.mu.Lock()
	eng := r.eng
	r.mu.Unlock()
	if eng == nil {
		return nil // not built yet; nothing to abort
	}
	eng.HandleAbort()
	return nil
}
