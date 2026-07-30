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
// These are real OS pipes (os.Pipe), not io.Pipe: io.Pipe is an unbuffered
// synchronous rendezvous, and agent.Frontend.Emit does two separate Write
// calls per frame (the JSON bytes, then a bare "\n"). Over io.Pipe, a reader
// that decodes one complete JSON value and then stops reading — which is
// exactly what a line/frame-oriented consumer does once it finds the frame it
// wanted — leaves that second Write blocked forever, wedging the engine
// goroutine and everything waiting on it. A real OS pipe has a kernel buffer
// (tens of KB), matching what a subprocess's stdio actually gives the daemon,
// so both Writes complete immediately regardless of reader pacing.
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
	r.mu.Unlock()

	go r.run(ctx, stdinR, stdoutW)

	// An in-process agent has no separate diagnostic stream: its logs go to the
	// daemon's logger tagged with the child id. Return a reader already at EOF
	// so readStderr completes immediately rather than being handed a nil.
	return stdinW, stdoutR, io.NopCloser(strings.NewReader("")), nil
}

// run owns the engine's whole lifetime. Deferred funcs are LIFO, so the
// recover-and-close defer registered first runs last: shutdown() and
// Engine.Close() complete before stdout closes, which is what makes the EOF the
// daemon sees mean "this child is finished".
func (r *Runner) run(ctx context.Context, stdinR *os.File, stdoutW *os.File) {
	defer close(r.done)
	defer func() {
		if v := recover(); v != nil {
			slog.Error("inproc: agent panicked",
				"child", r.opts.ChildID, "panic", v, "stack", string(debug.Stack()))
			r.setExit(2)
			// The engine's turn worker may still be running; stop it. Its
			// Close() is unreachable from here — an accepted leak on a path
			// that should never execute.
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

// Kill cancels the context and closes the read end of stdin, forcing
// Frontend.Run to return even if it is blocked on a read.
func (r *Runner) Kill() error {
	if err := r.Terminate(); err != nil {
		return err
	}
	r.mu.Lock()
	stdinR := r.stdinR
	r.mu.Unlock()
	if stdinR != nil {
		return stdinR.Close()
	}
	return nil
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
