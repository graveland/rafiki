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

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// mustDial connects to the daemon's UDS using the --socket flag value
// (default: client.DefaultSocketPath, the XDG runtime path). Exits with code 2
// on failure so connection errors are distinguishable from user-input errors
// (exit 1).
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
			return "", fmt.Errorf("no child specified and no active marker; run `rafiki list` to see options")
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

// resolvedSocket returns the concrete socket path this invocation talks to:
// the --socket flag if given, else whatever Dial would have picked. Unlike
// socketFromCmd it never returns "" — callers that must hand the path to
// another process cannot pass along "resolve it yourself", because that other
// process may resolve it differently.
func resolvedSocket(cmd *cobra.Command) string {
	if s := socketFromCmd(cmd); s != "" {
		return s
	}
	return client.DefaultSocketPath()
}

// findRafikiAttach returns the absolute path to the rafiki-attach binary.
// Looks first in the same directory as the running rafiki executable, then on PATH.
func findRafikiAttach() (string, error) {
	self, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(self), "rafiki-attach")
		if _, statErr := os.Stat(sibling); statErr == nil {
			return sibling, nil
		}
	}
	if path, lookErr := exec.LookPath("rafiki-attach"); lookErr == nil {
		return path, nil
	}
	return "", fmt.Errorf("rafiki-attach binary not found (expected sibling of rafiki or on PATH); install bun and run 'make build-attach'")
}

// attachEnv is the environment rafiki-attach is spawned with: ours, plus an
// explicit socket path so the TUI cannot resolve a different default than the
// one this process is talking to.
//
// Appending is enough to win over an inherited value: os/exec deduplicates
// Env and keeps the last entry for a repeated key.
func attachEnv(socket string) []string {
	return append(os.Environ(), paths.Socket+"="+socket)
}

// execRafikiAttach spawns rafiki-attach <childID> with stdio inherited and waits
// for it to exit. Returns when rafiki-attach exits. If rafiki-attach exits with a
// non-zero code, os.Exit is called directly so the exit code propagates
// without extra error noise (rafiki-attach has already printed to stderr).
//
// socket is passed down explicitly in the environment. Letting rafiki-attach
// resolve its own default was a live bug: the TS side never learned about the
// XDG move and fell back to ~/.pi/run/controller.sock, which is
// pi-controller's socket — so the TUI dialled the wrong daemon (or nothing)
// unless the user happened to export the socket path by hand. It also means
// --socket now reaches the TUI, which it previously did not.
func execRafikiAttach(childID, socket string) error {
	bin, err := findRafikiAttach()
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, childID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = attachEnv(socket)
	if runErr := cmd.Run(); runErr != nil {
		// rafiki-attach already wrote to stderr; just propagate the exit code.
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("rafiki-attach: %w", runErr)
	}
	return nil
}

// attachAndDecide runs rafiki-attach and, after it exits normally, prompts the
// user to keep or kill the session (unless overridden by flags or non-TTY
// stdin). SIGINT/SIGTERM during the subprocess skip the prompt — the user
// is forcibly exiting and the session should keep running (default keep).
func attachAndDecide(cmd *cobra.Command, childID string, killOnExit, keepOnExit bool) error {
	// Install handler before spawning so we catch any signal that kills both
	// rafiki and rafiki-attach. Buffer=1 so the send never blocks.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := execRafikiAttach(childID, resolvedSocket(cmd)); err != nil {
		// Even on subprocess error, defensively reset the terminal — rafiki-attach
		// may have crashed mid-render with raw mode / alt screen / kitty
		// keyboard protocol active, and the user is about to see Go-side output.
		resetTerminal()
		return err
	}

	// Defensively reset the terminal regardless of rafiki-attach's exit path.
	// Pi-tui's own teardown should handle this, but races on hard exit paths
	// (daemon broadcast, signal-driven shutdown, uncaught throws) have left
	// the terminal in advanced modes (kitty keyboard, modifyOtherKeys, etc.)
	// that the TS-side restoreTerminal missed.  This is a safety net.
	resetTerminal()

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

	// Re-dial: rafiki-attach's connection has already closed when it exited.
	c := mustDial(cmd)
	defer c.Close()

	// Use the same kill+forget policy as `rafiki kill` so a confirmed
	// terminate also removes the child from `rafiki list` on clean exit.
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
// children, skipping rafiki/ auto-labels.  Used for --label flag completion.
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
			if strings.HasPrefix(k, "rafiki/") {
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
// children, skipping rafiki/ auto-labels.  Used for --has-label flag completion.
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
			if strings.HasPrefix(k, "rafiki/") {
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

// resetTerminal writes a comprehensive set of escape sequences to restore the
// terminal to a sane interactive state.  Called by attachAndDecide after the
// rafiki-attach subprocess returns — a safety net in case the TS-side
// restoreTerminal missed something (kitty keyboard protocol races, etc.).
//
// Mirrors what `reset(1)` does for advanced terminal modes pi-tui activates:
//   - DECTCEM (?25h)       → show cursor
//   - ?1049l               → exit alternate screen buffer
//   - ?2004l               → disable bracketed paste
//   - ?1000l/?1002l/?1003l → disable mouse tracking (X11/cell/all-motion)
//   - ?1006l               → disable SGR-encoded mouse
//   - <u                   → pop kitty keyboard protocol stack (full reset)
//   - >4;0m                → disable xterm modifyOtherKeys
//   - 0m                   → reset SGR attributes
//
// All writes go to stdout (where the TUI was) regardless of rafiki-attach's
// stdout direction — rafiki's stdout is the terminal in this caller path.
func resetTerminal() {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	const seq = "\x1b[?25h" + // show cursor
		"\x1b[?1049l" + // exit alt screen
		"\x1b[?2004l" + // disable bracketed paste
		"\x1b[?1000l" + // disable X11 mouse
		"\x1b[?1002l" + // disable cell motion mouse
		"\x1b[?1003l" + // disable all-motion mouse
		"\x1b[?1006l" + // disable SGR-encoded mouse
		"\x1b[<u" + // pop kitty keyboard protocol stack
		"\x1b[>4;0m" + // disable xterm modifyOtherKeys
		"\x1b[0m" // reset SGR attributes
	_, _ = os.Stdout.WriteString(seq)
}

// knownEventTypes lists pi RPC event types used by --include/--exclude
// flags on tail and recent. Source: docs/reference/control-protocol.md §7 and §10.
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
