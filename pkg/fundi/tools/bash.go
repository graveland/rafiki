package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/agentloop"
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
	bashSchema = `{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to run via bash -c."},
			"timeout_ms": {"type": "integer", "description": "Timeout in milliseconds. Defaults to 120000 (120s); clamped to a 600000 (600s) maximum."}
		},
		"required": ["command"]
	}`
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
	return &bashTool{BashBlueprint: BashBlueprint{}, p: opts.OutputPolicy, cwd: opts.Cwd}, nil
}

type bashTool struct {
	BashBlueprint
	p        OutputPolicy
	cwd      string
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

	cmd := exec.CommandContext(cctx, "bash", "-c", in.Command)
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

	spillName := agentloop.ToolCallID(ctx)
	if spillName == "" {
		spillName = fmt.Sprintf("bash_%d", bt.fallback.Add(1))
	}
	return NewTextResult(bt.p.Clip(out, spillName)), nil
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

