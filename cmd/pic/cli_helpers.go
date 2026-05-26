package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

// mustDial connects to the daemon's UDS using the --socket flag value
// (default ~/.pi/run/controller.sock). Exits with code 2 on failure so
// connection errors are distinguishable from user-input errors (exit 1).
func mustDial(cmd *cobra.Command) *client.Client {
	socket, _ := cmd.Flags().GetString("socket")
	c, err := client.Dial(socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: connect:", err)
		os.Exit(2)
	}
	return c
}

// cmdCtx returns the cobra command's context (populated with cancellation
// on SIGINT etc by cobra.Command.ExecuteContext). Falls back to
// context.Background() if no context was set.
func cmdCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// outputOpts reads --output and --color from the command flags and returns
// the resolved output mode and whether color should be emitted.
func outputOpts(cmd *cobra.Command) (outputMode, bool) {
	outFlag, _ := cmd.Flags().GetString("output")
	colorFlag, _ := cmd.Flags().GetString("color")
	tty := isStdoutTTY()
	return resolveOutputMode(outFlag, tty), colorEnabled(colorFlag, tty)
}

// resolveTarget returns the resolved childID for a subcommand argument.
// If input is empty it falls back to the active marker file; if that is
// also absent it returns a clear error.
func resolveTarget(ctx context.Context, c *client.Client, input string) (string, error) {
	if input == "" {
		input = getActive()
		if input == "" {
			return "", fmt.Errorf("no child specified and no active marker; run `pic list` to see options")
		}
	}
	return c.Resolve(ctx, input)
}

// completeChildren returns child IDs and names that start with toComplete.
// Used by Cobra dynamic-completion handlers. Swallows all errors so that
// tab completion gracefully no-ops when the daemon is down.
func completeChildren(cmd *cobra.Command, toComplete string) []string {
	c, err := client.Dial(socketFromCmd(cmd))
	if err != nil {
		return nil
	}
	defer c.Close()

	children, err := c.List(cmdCtx(cmd), protocol.ListFilter{})
	if err != nil {
		return nil
	}

	var out []string
	for _, ch := range children {
		if strings.HasPrefix(ch.ChildID, toComplete) {
			out = append(out, ch.ChildID)
		}
		if ch.Name != "" && strings.HasPrefix(ch.Name, toComplete) {
			out = append(out, ch.Name)
		}
	}
	return out
}

// socketFromCmd extracts the --socket flag value from the command (or its
// ancestors). Returns "" if the flag is not set, which Dial treats as the
// default path.
func socketFromCmd(cmd *cobra.Command) string {
	s, _ := cmd.Flags().GetString("socket")
	return s
}

// findPicAttach returns the absolute path to the pic-attach binary.
// Looks first in the same directory as the running pic executable, then on PATH.
func findPicAttach() (string, error) {
	self, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(self), "pic-attach")
		if _, statErr := os.Stat(sibling); statErr == nil {
			return sibling, nil
		}
	}
	if path, lookErr := exec.LookPath("pic-attach"); lookErr == nil {
		return path, nil
	}
	return "", fmt.Errorf("pic-attach binary not found (expected sibling of pic or on PATH); install bun and run 'make build-attach'")
}

// execPicAttach spawns pic-attach <childID> with stdio inherited and waits
// for it to exit. Returns when pic-attach exits. If pic-attach exits with a
// non-zero code, os.Exit is called directly so the exit code propagates
// without extra error noise (pic-attach has already printed to stderr).
func execPicAttach(childID string, killOnExit bool) error {
	bin, err := findPicAttach()
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, childID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if killOnExit {
		cmd.Env = append(cmd.Env, "PIC_KILL_ON_EXIT=1")
	}
	if runErr := cmd.Run(); runErr != nil {
		// pic-attach already wrote to stderr; just propagate the exit code.
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("pic-attach: %w", runErr)
	}
	return nil
}

// knownEventTypes lists pi RPC event types used by --include/--exclude
// flags on tail and recent. Source: tasks/pi-controller-protocol.md §7 and §10.
var knownEventTypes = []string{
	"agent_start", "agent_end",
	"turn_start", "turn_end",
	"message_start", "message_update", "message_end",
	"tool_execution_start", "tool_execution_update", "tool_execution_end",
	"queue_update",
	"compaction_start", "compaction_end",
	"auto_retry_start", "auto_retry_end",
	"extension_error",
	"extension_ui_request",
	// ctrl_child_* events used by the lifecycle profile
	"ctrl_child_spawned",
	"ctrl_child_exited",
	"ctrl_child_status",
	"ctrl_child_renamed",
}
