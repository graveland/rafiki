package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

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
	return completeChildrenByState(cmd, toComplete, func(_ protocol.ChildSummary) bool {
		return true
	})
}

// completeChildrenByState is like completeChildren but filters candidates by
// the given predicate. Use it to restrict completions to a relevant subset
// (e.g. only exited children for `forget`, only live ones for `kill`).
func completeChildrenByState(cmd *cobra.Command, toComplete string, keep func(protocol.ChildSummary) bool) []string {
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
		if !keep(ch) {
			continue
		}
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
func execPicAttach(childID string) error {
	bin, err := findPicAttach()
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, childID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if runErr := cmd.Run(); runErr != nil {
		// pic-attach already wrote to stderr; just propagate the exit code.
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("pic-attach: %w", runErr)
	}
	return nil
}

// attachAndDecide runs pic-attach and, after it exits normally, prompts the
// user to keep or kill the session (unless overridden by flags or non-TTY
// stdin). SIGINT/SIGTERM during the subprocess skip the prompt — the user
// is forcibly exiting and the session should keep running (default keep).
func attachAndDecide(cmd *cobra.Command, childID string, killOnExit, keepOnExit bool) error {
	// Install handler before spawning so we catch any signal that kills both
	// pic and pic-attach. Buffer=1 so the send never blocks.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := execPicAttach(childID); err != nil {
		return err
	}

	// Stop delivery before the non-blocking drain so there's no race between
	// signal.Stop and the select.
	signal.Stop(sigCh)
	select {
	case <-sigCh:
		// Signal-driven exit: skip prompt, keep session.
		return nil
	default:
	}

	shouldKill, err := decideKillOnExit(killOnExit, keepOnExit, childID)
	if err != nil {
		return err
	}
	if !shouldKill {
		return nil
	}

	// Re-dial: pic-attach's connection has already closed when it exited.
	c := mustDial(cmd)
	defer c.Close()

	// Use the same kill+forget policy as `pic kill` so a confirmed
	// terminate also removes the child from `pic list` on clean exit.
	res, err := killAndMaybeForget(cmdCtx(cmd), c, childID, 0, 0, false)
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	if res.ForgetErr != nil {
		fmt.Fprintf(os.Stderr, "warning: forget after kill failed: %v\n", res.ForgetErr)
	}
	return nil
}

// decideKillOnExit returns true if the session should be killed at exit.
//
//   - --kill-on-exit → true (no prompt)
//   - --keep-on-exit → false (no prompt)
//   - non-TTY stdin → false (no human present to answer)
//   - otherwise → prompt the user; default answer (Enter) is keep
func decideKillOnExit(killOnExit, keepOnExit bool, childLabel string) (bool, error) {
	if killOnExit {
		return true, nil
	}
	if keepOnExit {
		return false, nil
	}
	if !isStdinTTY() {
		return false, nil
	}

	fmt.Printf("\nTerminate session %q? [y/N]: ", childLabel)

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	ans := strings.TrimSpace(strings.ToLower(line))
	kill, warned := parseKillAnswer(ans)
	if warned {
		fmt.Println("(treating as no)")
	}
	return kill, nil
}

// parseKillAnswer interprets the user's trimmed, lowercased response.
// Returns (kill=true) only for explicit "y" / "yes".
// Returns (warned=true) when the input is unrecognised so the caller can
// print a "(treating as no)" notice.
func parseKillAnswer(ans string) (kill bool, warned bool) {
	switch ans {
	case "y", "yes":
		return true, false
	case "", "n", "no":
		return false, false
	default:
		return false, true
	}
}

// isStdinTTY reports whether stdin is connected to a terminal.
func isStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// completeLabelPairs returns "k=v" completions from all currently-known
// children, skipping pic/ auto-labels.  Used for --label flag completion.
func completeLabelPairs(cmd *cobra.Command, toComplete string) []string {
	c, err := client.Dial(socketFromCmd(cmd))
	if err != nil {
		return nil
	}
	defer c.Close()
	children, err := c.List(cmdCtx(cmd), protocol.ListFilter{})
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, ch := range children {
		for k, v := range ch.Labels {
			if strings.HasPrefix(k, "pic/") {
				continue
			}
			pair := k + "=" + v
			if _, ok := seen[pair]; ok {
				continue
			}
			if strings.HasPrefix(pair, toComplete) {
				out = append(out, pair)
			}
			seen[pair] = struct{}{}
		}
	}
	return out
}

// completeLabelKeys returns label key completions from all currently-known
// children, skipping pic/ auto-labels.  Used for --has-label flag completion.
func completeLabelKeys(cmd *cobra.Command, toComplete string) []string {
	c, err := client.Dial(socketFromCmd(cmd))
	if err != nil {
		return nil
	}
	defer c.Close()
	children, err := c.List(cmdCtx(cmd), protocol.ListFilter{})
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, ch := range children {
		for k := range ch.Labels {
			if strings.HasPrefix(k, "pic/") {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			if strings.HasPrefix(k, toComplete) {
				out = append(out, k)
			}
			seen[k] = struct{}{}
		}
	}
	return out
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
