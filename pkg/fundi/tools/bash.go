package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/toolmeta"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 600 * time.Second
	bashWaitDelay      = 5 * time.Second

	bashDescription = "Run a shell command via `bash -c` in the working directory. " +
		"stdout and stderr are merged into one result. A non-zero exit code is " +
		"reported as text in the result, not as a tool error. Defaults to a " +
		"120s timeout; pass timeout_ms to override, up to a 600s maximum. " +
		"Large output is clipped (head and tail kept, middle elided) with the " +
		"full output spilled to a file whose path is named in the result."
)

func init() { DefaultBlueprint.Register(&BashBlueprint{}) }

type BashBlueprint struct{}

func (BashBlueprint) Name() string        { return "bash" }
func (BashBlueprint) Description() string { return bashDescription }
func (BashBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "command", Type: "string", Description: "Shell command to run via bash -c."},
			{Name: "timeout_ms", Type: "integer", Description: "Timeout in milliseconds. Defaults to 120000 (120s); clamped to a 600000 (600s) maximum."},
		},
		Required: []string{"command"},
	}
}
func (BashBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (BashBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	bt := &bashTool{
		BashBlueprint: BashBlueprint{},
		p:             opts.OutputPolicy,
		cwd:           opts.Cwd,
		rtkMode:       opts.RTK,
	}
	return bt, nil
}

type bashTool struct {
	BashBlueprint
	p        OutputPolicy
	cwd      string
	rtkMode  RTKMode
	fallback atomic.Int64
}

func (bt *bashTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in bashInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("bash: invalid input: %w", err)
	}
	if in.Command == "" {
		return ToolResult{}, fmt.Errorf("bash: command is required")
	}

	timeout := bashTimeout(in.TimeoutMs)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Try rtk rewrite first. When applied, exec the rtk argv directly
	// instead of wrapping in bash -c.
	rtkArgv, rtkApplied := rtkRewrite(bt.rtkMode, in.Command)
	var argv0 string
	var argvRest []string
	if rtkApplied {
		argv0, argvRest = rtkArgv[0], rtkArgv[1:]
	} else {
		argv0, argvRest = "bash", []string{"-c", in.Command}
	}

	out, stderrText, runErr := bt.run(cctx, argv0, argvRest)

	// rtk's rewrite-guard design note says a REFUSAL TO REWRITE costs
	// nothing — it always falls back to plain bash. But once a command has
	// actually been rewritten, a refusal by rtk itself is a different
	// failure mode: it surfaces to the model as an opaque command failure
	// for something plain bash would have run fine. Confirmed against the
	// installed rtk (0.43.0):
	//
	//	find . -name '*.go' -not -path './vendor/*'
	//
	// has every metacharacter single-quoted, so hasShellChaining correctly
	// leaves it alone, it rewrites to `rtk find …`, and rtk exits 1 with
	// stderr "rtk: rtk find does not support compound predicates or
	// actions (e.g. -not, -exec). Use `find` directly." — a command that
	// plain bash executes without complaint.
	//
	// The fallback is deliberately narrow: only re-run when rtkRefused is
	// confident rtk itself declined, never merely because the exit code was
	// nonzero. Exit code alone cannot tell "rtk refused" from "the
	// underlying tool failed" — git outside a repo, kubectl against a
	// missing pod, and rtk's own refusals all exit nonzero, with no shared
	// code across them (128, 1, 1 respectively were observed). Re-running
	// on a genuine tool failure would execute something like a rejected
	// `git push` a second time, so this errs hard toward NOT falling back.
	if rtkApplied && isNonZeroExit(runErr) && rtkRefused(stderrText) {
		slog.Warn("agent/tools: bash: rtk refused the rewritten command, falling back to plain bash",
			"command", in.Command, "rtk_stderr", stderrText)
		out, _, runErr = bt.run(cctx, "bash", []string{"-c", in.Command})
	}

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
				slog.Warn("agent/tools: bash: wait delay expired, background processes still hold the output pipes",
					"command", in.Command, "error", runErr)
				out += "\n[bash: command exited but left background processes holding its output pipes]\n"
			case errors.As(runErr, &startErr),
				errors.Is(runErr, exec.ErrNotFound),
				errors.Is(runErr, os.ErrPermission):
				return ToolResult{}, fmt.Errorf("bash: %w", runErr)
			default:
				slog.Error("agent/tools: bash: unexpected error running command",
					"command", in.Command, "error", runErr)
				out += fmt.Sprintf("\n[bash: %v]\n", runErr)
			}
		}
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = fmt.Sprintf("bash_%d", bt.fallback.Add(1))
	}
	return NewTextResult(bt.p.Clip(out, spillName)), nil
}

// syncWriter is a mutex-guarded io.Writer. exec.Cmd services Stdout and
// Stderr from two SEPARATE goroutines unless the two fields are the exact
// same writer (that's what exec.Cmd.CombinedOutput actually relies on:
// pointing both fields at one *bytes.Buffer, which Cmd special-cases via an
// identity check to share a single pipe). run needs stderr written to two
// destinations — the merged buffer and a stderr-only copy for rtkRefused —
// so Stdout and Stderr can no longer be identical writers, and a bare
// *bytes.Buffer behind an io.MultiWriter would then be hit by both
// goroutines concurrently: not a benign interleaving question but a real
// data race, observed as stderr text simply missing from the result
// (`echo out; echo err >&2; exit 3` produced "out\n\nexit status 3\n", no
// "err" anywhere) rather than corrupted or reordered.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// run execs name(args...) under ctx with this tool's cwd, its process-group
// kill-on-cancel, and its WaitDelay all wired up identically regardless of
// whether the caller is running the rtk-rewritten argv or a plain
// `bash -c`. It returns the merged stdout+stderr text (the model-facing
// result) and stderr alone (so rtkRefused can inspect it without stdout
// noise) — see syncWriter for why this isn't just CombinedOutput plus a tee.
func (bt *bashTool) run(ctx context.Context, name string, args []string) (combined, stderrOnly string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = bt.cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = bashWaitDelay

	combinedBuf := &syncWriter{}
	var stderrBuf bytes.Buffer // written by stderr's copy goroutine only — no race
	cmd.Stdout = combinedBuf
	cmd.Stderr = io.MultiWriter(combinedBuf, &stderrBuf)
	err = cmd.Run()
	return combinedBuf.String(), stderrBuf.String(), err
}

// rtkRefused reports whether stderr looks like RTK ITSELF refusing to run
// the rewritten command, as opposed to the underlying tool failing on its
// own. Verified against the installed rtk (0.43.0):
//
//	rtk find . -name '*.go' -not -path './vendor/*'
//
// exits 1 with stderr "rtk: rtk find does not support compound predicates
// or actions (e.g. -not, -exec). Use `find` directly." — rtk prefixes its
// own diagnostics with "rtk: ". A real underlying-tool failure passes the
// tool's own stderr through untouched, with no such prefix: `rtk git
// status` outside a repo exits 128 with "fatal: not a git repository…";
// `rtk docker <bogus-subcommand>` exits 1 with "docker: unknown command:
// …"; `rtk aws s3 cp` with a bad flag exits 255 with "aws: [ERROR]: …".
// Exit codes overlap across both cases (1, 128, 254, 255 were all
// observed on refusals AND on genuine failures), so they cannot be used to
// tell the two apart — only the prefix can.
func rtkRefused(stderr string) bool {
	return strings.HasPrefix(strings.TrimSpace(stderr), "rtk: ")
}

// isNonZeroExit reports whether err represents the command actually
// running and exiting nonzero, as opposed to a timeout, a cancellation, or
// a failure to start at all. Only a genuine nonzero exit is eligible for
// the rtk-refusal fallback — the other cases mean there is no real result
// to compare rtk's stderr against, and re-running under those conditions
// (e.g. an already-expired timeout) would just fail again for an unrelated
// reason.
func isNonZeroExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

type bashInput struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}

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
