package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"git.graveland.dev/brent/rafiki/agentloop"
)

const (
	// defaultBashTimeout is applied when timeout_ms is omitted or <= 0.
	defaultBashTimeout = 120 * time.Second
	// maxBashTimeout caps timeout_ms — a caller cannot ask for a longer run
	// than this no matter what it passes.
	maxBashTimeout = 600 * time.Second
	// bashWaitDelay bounds how long Wait spends on unclosed I/O pipes after
	// the process has been signaled to die (via ctx cancellation or the
	// derived timeout), so a child that ignores the kill signal or leaves
	// pipes open can't hang the tool call forever.
	bashWaitDelay = 5 * time.Second

	bashDescription = "Run a shell command via `bash -c` in the working directory. " +
		"stdout and stderr are merged into one result. A non-zero exit code is " +
		"reported as text in the result, not as a tool error. Defaults to a " +
		"120s timeout; pass timeout_ms to override, up to a 600s maximum. " +
		"Large output is clipped (head and tail kept, middle elided) with the " +
		"full output spilled to a file whose path is named in the result."
	bashSchema = `{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to run via bash -c."},
			"timeout_ms": {"type": "integer", "description": "Timeout in milliseconds. Defaults to 120000 (120s); clamped to a 600000 (600s) maximum."}
		},
		"required": ["command"]
	}`
)

type bashInput struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}

// bashTimeout resolves timeout_ms to an actual duration: <=0 falls back to
// defaultBashTimeout, anything over maxBashTimeout is clamped to it.
//
// The clamp happens in MILLISECOND space, before the multiply. Converting
// first (time.Duration(timeoutMs) * time.Millisecond) overflows int64 for
// large timeout_ms values and wraps to a negative duration, which sails
// through a `> maxBashTimeout` check and yields an already-expired context —
// turning "give me a very long timeout" into "kill it immediately".
func bashTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return defaultBashTimeout
	}
	const maxMs = int64(maxBashTimeout / time.Millisecond)
	if int64(timeoutMs) >= maxMs {
		return maxBashTimeout
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

// RegisterBash registers the bash tool against r. Results are passed
// through p.Clip before being handed to the model, spilling full output to
// p.SpillDir when it exceeds p.Budget. cwd is the working directory every
// command runs in.
func RegisterBash(r *Registry, p OutputPolicy, cwd string) {
	// Fallback naming for spilled output when agentloop.ToolCallID(ctx) is
	// empty (e.g. a direct Execute call outside a real agentloop turn).
	// Tools run concurrently (errgroup, limit 6 — see Registry), so this
	// must be race-safe; atomic.Int64 gives each call a distinct name
	// without a mutex.
	var fallbackCounter atomic.Int64

	r.Register(Def("bash", bashDescription, bashSchema), func(ctx context.Context, input json.RawMessage) (string, error) {
		var in bashInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("bash: invalid input: %w", err)
		}
		if in.Command == "" {
			return "", fmt.Errorf("bash: command is required")
		}

		timeout := bashTimeout(in.TimeoutMs)

		// Derived from ctx (not context.Background()): when the caller
		// cancels ctx — the in-band abort path — this derived context is
		// canceled too, which is what drives cmd.Cancel below. A bash tool
		// that timed out on its own context would silently ignore abort.
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(cctx, "bash", "-c", in.Command)
		cmd.Dir = cwd
		// Put the shell in its own process group so cancellation can kill
		// the whole tree, not just the shell. bash FORKS (rather than
		// exec's) for pipelines, `&&`/`;` chains, and background jobs — i.e.
		// for essentially every non-trivial command — so killing the direct
		// pid leaves the actual expensive work (`go test ./...`) running.
		// Those orphans also inherit the stdout/stderr pipes, so Wait would
		// then block for the full WaitDelay waiting on pipes nobody is going
		// to close. Killing the group fixes both at once.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Explicit: really kill the process group (SIGKILL) the moment cctx
		// is done, whether that's our own timeout or the caller's abort.
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			// Negative pid == "the process group led by pid". Setpgid above
			// makes the shell its own group leader, so this can never reach
			// our own group.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					// Whole group already gone; exec treats this as a
					// benign race rather than a cancellation failure.
					return os.ErrProcessDone
				}
				return err
			}
			return nil
		}
		// Bound how long we wait for a killed-but-slow-to-exit process (or
		// unclosed I/O pipes) before giving up — this is the guarantee that
		// abort actually terminates the call promptly rather than hanging.
		cmd.WaitDelay = bashWaitDelay

		outBytes, runErr := cmd.CombinedOutput()
		out := string(outBytes)

		if runErr != nil {
			switch {
			case errors.Is(cctx.Err(), context.DeadlineExceeded):
				out += fmt.Sprintf("\n[bash: command timed out after %s and was killed]\n", timeout)
			case errors.Is(cctx.Err(), context.Canceled):
				out += "\n[bash: command was canceled and killed]\n"
			default:
				var exitErr *exec.ExitError
				var startErr *exec.Error
				switch {
				case errors.As(runErr, &exitErr):
					out += "\n" + exitErr.Error() + "\n"
				case errors.Is(runErr, exec.ErrWaitDelay):
					// The command itself finished, but something it
					// backgrounded (`cmd &`, nohup, a spawned dev server)
					// still holds the inherited stdout/stderr pipes, so Wait
					// gave up on the copy after WaitDelay. This is NOT a
					// failure to run: exit status was fine and outBytes holds
					// real output. Per "spill, never destroy" that output is
					// returned with a note, never discarded behind an error.
					slog.Warn("agent/tools: bash: wait delay expired, background processes still hold the output pipes",
						"command", in.Command, "error", runErr)
					out += "\n[bash: command exited but left background processes holding its output pipes]\n"
				case errors.As(runErr, &startErr),
					errors.Is(runErr, exec.ErrNotFound),
					errors.Is(runErr, os.ErrPermission):
					// Didn't even start (bash missing, not executable, ...) —
					// a genuine tool failure, and there is no output to lose.
					return "", fmt.Errorf("bash: %w", runErr)
				default:
					// Unclassified: the command may well have produced
					// output, so surface the error as text rather than
					// trading collected bytes for an error string.
					slog.Error("agent/tools: bash: unexpected error running command",
						"command", in.Command, "error", runErr)
					out += fmt.Sprintf("\n[bash: %v]\n", runErr)
				}
			}
		}

		spillName := agentloop.ToolCallID(ctx)
		if spillName == "" {
			spillName = fmt.Sprintf("bash_%d", fallbackCounter.Add(1))
		}
		return p.Clip(out, spillName), nil
	})
}
