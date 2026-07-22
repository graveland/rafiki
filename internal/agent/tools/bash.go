package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync/atomic"
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
func bashTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return defaultBashTimeout
	}
	d := time.Duration(timeoutMs) * time.Millisecond
	if d > maxBashTimeout {
		return maxBashTimeout
	}
	return d
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
		// Explicit: really kill the process (SIGKILL) the moment cctx is
		// done, whether that's our own timeout or the caller's abort.
		cmd.Cancel = func() error {
			return cmd.Process.Kill()
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
				if errors.As(runErr, &exitErr) {
					out += "\n" + exitErr.Error() + "\n"
				} else {
					// Didn't even start (bad Path, permission denied, ...) —
					// that's a genuine tool failure, not a command result.
					return "", fmt.Errorf("bash: %w", runErr)
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
