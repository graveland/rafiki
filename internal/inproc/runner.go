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

// stopReason records why this runner is coming down, so run() can map a
// Frontend.Run error onto the right exit contract instead of treating every
// one of them as a generic failure.
type stopReason int

const (
	// stopNone: nothing deliberate happened. A Frontend.Run error here is a
	// genuine failure and exits 1.
	stopNone stopReason = iota
	// stopEngineFatal: the engine's turn worker hit a panic and asked to be
	// ended (see Runner.engineFatal). The exit code is already recorded.
	stopEngineFatal
	// stopKilled: Kill() forced this teardown. Reported as a signalled stop,
	// not a failure — see killedSignal.
	stopKilled
)

func (s stopReason) String() string {
	switch s {
	case stopEngineFatal:
		return "engine_fatal"
	case stopKilled:
		return "killed"
	default:
		return "none"
	}
}

// killedSignal is the signal name a Kill()-initiated teardown reports.
//
// It must stay byte-identical to what a SIGKILLed subprocess produces:
// processRunner.Wait reads syscall.WaitStatus.Signal().String(), and
// syscall.SIGKILL stringifies to exactly "killed". The two runners are two
// halves of one wire contract — ChildSummary.ExitCode/ExitSignal,
// KillResponseData.Signal, and meta.json all carry whichever one served the
// child — so a user killing an agent child and a user killing a pi child must
// not get different answers for the same action.
const killedSignal = "killed"

// Runner drives an agent.Engine in a goroutine. It satisfies child.Runner.
type Runner struct {
	opts Options
	done chan struct{}

	mu         sync.Mutex
	started    bool
	stop       stopReason
	cancel     context.CancelFunc
	exitCode   int
	exitSignal string
	eng        *agent.Engine
	stdinR     *os.File
	stdoutR    *os.File
}

// New returns a Runner for opts. Nothing runs until Start is called.
func New(opts Options) *Runner {
	if opts.Build == nil {
		opts.Build = agent.BuildRuntime
	}
	if opts.Parent == nil {
		opts.Parent = context.Background()
	}
	r := &Runner{opts: opts, done: make(chan struct{})}
	// The runner OWNS the engine's fatal hook, so it is installed here rather
	// than accepted from the caller: this Runner is the only thing that can
	// act on it (record the exit, unblock Frontend.Run so stdout closes and
	// the daemon sees an EOF), and a caller-supplied hook would be silently
	// pointing at nothing. A Build override in a test receives it on the
	// RuntimeOptions it is handed and may forward it to EngineConfig.
	r.opts.Runtime.OnFatal = r.engineFatal
	return r
}

// engineFatal is EngineConfig.OnFatal for this child: the engine calls it when
// a turn panicked and its worker has stopped for good. By contract the engine
// has already released every outstanding Wait() count before calling, so
// eng.Wait() in run() cannot block on the turn that died.
//
// Ending the child means unblocking Frontend.Run, which is parked in a read on
// stdinR. Closing that read end is the only way to do it — the same mechanism
// Kill uses, minus the stdout close, because on this path the engine is not
// wedged in a write and the daemon should still receive the frames already
// queued (including the agent_error the engine emits on its way out).
func (r *Runner) engineFatal(err error) {
	slog.Error("inproc: engine reported a fatal error; ending the child",
		"child", r.opts.ChildID, "error", err)

	r.mu.Lock()
	r.stop = stopEngineFatal
	r.exitCode = 2 // same code run()'s own recover records for a panic
	cancel := r.cancel
	stdinR := r.stdinR
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stdinR != nil {
		if cerr := stdinR.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			slog.Warn("inproc: close stdin reader after engine fatal", "child", r.opts.ChildID, "error", cerr)
		}
	}
}

// stopReasonOf reports why this runner is stopping.
func (r *Runner) stopReasonOf() stopReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stop
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
//
// Start is single-shot and returns an error on any subsequent call. This is
// not defensive tidiness: a second Start would launch a second run()
// goroutine over the same Runner, and the second one's `defer close(r.done)`
// would panic on an already-closed channel. That panic is registered BEFORE
// run's recover defer, so it is not contained by it — it would take the whole
// daemon down. Unreachable through child.Spawn today (which calls Start
// exactly once per Runner and discards the Runner on failure), but the blast
// radius is out of all proportion to the cost of the guard.
func (r *Runner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil, nil, nil, errors.New("inproc: Start called more than once on the same Runner")
	}
	// Set before the pipes are created, not after they succeed: a Runner whose
	// Start failed partway has already been rejected by its caller, and
	// allowing a retry buys nothing while reopening the double-run() window.
	r.started = true
	r.mu.Unlock()

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
//     before close(r.done). It contains panic recovery and unconditionally
//     closes stdoutW, which is what turns a contained panic into an ordinary
//     EOF for the daemon's reader. Read the next paragraph before trusting it
//     as a general containment boundary: it is not one.
//  3. defer stdinR.Close() — registered BEFORE Build is called, deliberately.
//     Being ahead of Build is precisely what makes it fire on the build-error
//     return and on a panic raised inside Build itself; moving it after Build,
//     or making it conditional on Build succeeding, reintroduces an FD leak on
//     both of those paths. It always runs before #2 sees stdoutW close.
//  4. defer shutdown() — registered only after Build succeeds, so it never
//     runs on a build-error exit. Runs before #3's stdinR.Close().
//
// Net effect: shutdown() and Engine.Close() (called explicitly, near the end
// of this function body) both complete before stdout closes, which is what
// makes the EOF the daemon sees on stdout mean "this child is finished".
//
// # What this recover does and does not cover
//
// A recover only ever sees panics on ITS OWN goroutine. This one is on the
// run() goroutine, so it covers exactly: Options.Build (including
// agent.BuildRuntime and everything it constructs), fe.Run's reader loop and
// the Handler dispatch it makes inline (HandlePrompt/HandleSteer/HandleAbort/
// State — all of which return promptly by contract), eng.Wait, eng.Close, and
// shutdown().
//
// It does NOT cover, and cannot:
//
//   - The engine's turn worker. Every turn — agentloop.Run, the streaming
//     handler, message mapping — runs there. It has its OWN recover
//     (agent.Engine.runTurnGuarded), which routes a panic back here through
//     EngineConfig.OnFatal (see Runner.engineFatal) so the child ends with a
//     non-zero exit and a clean stdout EOF.
//   - agentloop's per-tool errgroup goroutines. errgroup does not recover.
//     Covered instead at the two fundi-owned boundaries that run there:
//     tools.Registry.Execute (the whole tool surface, MCP included) and the
//     OnToolStart/OnToolEnd emit callbacks in agent.Engine.events.
//   - The model-catalog warm goroutine, which recovers for itself in
//     agent.NewEngine.
//   - Goroutines inside dependencies, which no fundi-owned recover can reach:
//     os/exec's stdio copy goroutines (bash and every other tool subprocess),
//     the MCP client's session goroutines, and pgx's background pool
//     goroutines. A panic in any of those still takes the daemon down. That is
//     an accepted, unclosable gap in the in-process model, not an oversight.
func (r *Runner) run(ctx context.Context, stdinR *os.File, stdoutW *os.File) {
	defer close(r.done)
	defer func() {
		if v := recover(); v != nil {
			slog.Error("inproc: agent panicked",
				"child", r.opts.ChildID, "panic", v, "stack", string(debug.Stack()))
			r.setExit(2)
			// The engine's turn worker (if Build got far enough to create one)
			// may still be running. r.eng is readable here (under mu), but
			// calling Close() on it is NOT safe: Close's contract requires
			// Wait() to have already returned, and Wait() can block
			// indefinitely on a genuinely wedged turn — exactly what we cannot
			// risk doing from inside panic recovery, whose whole job is to
			// return promptly. Left as an accepted, bounded leak (one
			// goroutine) on a path that should never execute in practice. The
			// unconditional cancel() below still unblocks anything in that
			// worker that selects on ctx.
		}
		// Release the context derived from r.opts.Parent on EVERY exit path,
		// not just the panic one. This is not merely tidy. When Parent is a
		// real cancelCtx — and it always is in the daemon, which passes
		// Controller.baseCtx — the child ctx registers itself in Parent's
		// children map at WithCancel time and is removed only by its own
		// cancel. A run() that returned normally without cancelling therefore
		// pinned one cancelCtx per completed child for the daemon's entire
		// lifetime: an unbounded leak. No test could see it, because New
		// defaults Parent to context.Background(), whose nil Done() channel
		// makes propagateCancel register nothing at all.
		r.mu.Lock()
		cancel := r.cancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
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
		// An error here is not automatically a failure: a deliberate teardown
		// closes stdinR out from under the parked read, so Frontend.Run
		// surfaces os.ErrClosed rather than the EOF a clean stop produces.
		// Attributing that to the child would report a failing exit for an
		// action the daemon itself took.
		if reason := r.stopReasonOf(); reason == stopNone {
			slog.Error("inproc: frontend run", "child", r.opts.ChildID, "error", runErr)
			r.setExit(1)
		} else {
			slog.Info("inproc: frontend run ended by a deliberate stop",
				"child", r.opts.ChildID, "reason", reason, "error", runErr)
		}
	}
	eng.Wait()
	eng.Close()

	// Shape a forced stop's exit LAST, once everything else has settled. Kill
	// can land at two quite different moments — while Frontend.Run is parked
	// on a read (handled by the branch above) or after it already returned
	// cleanly, when the daemon's shutdown ladder escalated because the engine
	// was wedged mid-turn — and both must report the same thing.
	if r.stopReasonOf() == stopKilled {
		r.setExitSignal(0, killedSignal)
	}
}

func (r *Runner) setExit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exitCode = code
}

func (r *Runner) setExitSignal(code int, signal string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exitCode = code
	r.exitSignal = signal
}

// Wait blocks until the engine goroutine finishes and reports the exit the
// same way a subprocess runner does.
//
// The signal is empty for every ordinary end (clean EOF, build failure,
// frontend error, contained panic) and "killed" when Kill() forced the
// teardown — matching processRunner, which reports a SIGKILLed child as exit
// code 0 with signal "killed". Terminate() alone does not produce a signal: it
// only cancels the in-flight turn and the runner still ends on the daemon's
// stdin close, which is an ordinary EOF.
func (r *Runner) Wait() (int, string) {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode, r.exitSignal
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
//
// Kill also records that the stop was FORCED, which is what lets run() report
// the subprocess exit shape (code 0, signal "killed") instead of the exit 1 a
// bare Frontend.Run error would otherwise produce — closing stdinR makes that
// read fail with os.ErrClosed rather than returning the EOF a clean stop
// gives. Without this the two runners answer the same user action ("kill this
// child") differently on the wire. See killedSignal.
func (r *Runner) Kill() error {
	if err := r.Terminate(); err != nil {
		return err
	}
	r.mu.Lock()
	// Do not overwrite an engine-fatal stop: that child was already dying of
	// its own panic with a recorded exit code, and a Kill racing its teardown
	// should not rewrite the cause of death. A subprocess behaves the same way
	// — a signal delivered to an already-exited process is reaped as the
	// original exit, not as a kill.
	if r.stop == stopNone {
		r.stop = stopKilled
	}
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
