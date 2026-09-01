package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/paths"
)

// remoteDialURL returns RAFIKI_URL when it names a remote control plane
// (client.IsRemoteURL — https:// only), else "". This is the ONE gate mustDial
// and dialDaemon both dial through: an http:// RAFIKI_URL is the local
// loopback face, which has no control listener, and treating it as remote
// would send every CLI command in an installation that only ever set the
// documented proxy URL (.env.example's RAFIKI_URL=http://localhost:8035)
// straight into a TLS dial against a plaintext port. Centralised rather than
// duplicated at each call site so the two can never drift on which scheme
// counts as remote.
func remoteDialURL() string {
	u := paths.Get(paths.URL)
	if client.IsRemoteURL(u) {
		return u
	}
	return ""
}

// mustDial connects to the daemon. An https:// RAFIKI_URL dials the remote
// daemon's shared TLS listener; anything else (an http:// loopback face URL,
// or none) uses the local UDS, which the --socket flag overrides. Exits with
// code 2 on failure so connection errors stay distinguishable from
// user-input errors (exit 1).
func mustDial(cmd *cobra.Command) *client.Client {
	if u := remoteDialURL(); u != "" {
		c, err := client.DialURL(cmdCtx(cmd), u, paths.TokenFromEnv())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: connect %s: %v\n", u, err)
			os.Exit(2)
		}
		return c
	}
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
func completeChildren(cmd *cobra.Command, toComplete string) []string {
	return completeChildrenByState(cmd, toComplete, func(completionChild) bool {
		return true
	})
}

// completeChildrenByState is like completeChildren but filters candidates by
// the given predicate. Use it to restrict completions to a relevant subset
// (e.g. only exited children for `close`, only live ones for `kill`).
func completeChildrenByState(cmd *cobra.Command, toComplete string, keep func(completionChild) bool) []string {
	var out []string
	for _, ch := range completionChildren(cmd) {
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

// attachAndDecide runs the in-process TUI and, after it exits normally, prompts
// the user to keep or kill the session (unless overridden by flags or non-TTY
// stdin). SIGINT/SIGTERM during the TUI skip the prompt — the user
// is forcibly exiting and the session should keep running (default keep).
func attachAndDecide(cmd *cobra.Command, ep connectEndpoint, childID string, killOnExit, keepOnExit bool) error {
	// Install handler before starting the TUI so we catch any signal that
	// kills the process mid-render. Buffer=1 so the send never blocks.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// In-process now. This used to exec a `rafiki-attach` binary built from
	// TypeScript; B4 deleted the source and the make target, so the exec path
	// failed at runtime telling the user to run `make build-attach`.
	if err := runTUIForChild(cmd, ep, childID); err != nil {
		// Defensively reset the terminal — bubbletea may have died mid-render
		// with raw mode or the alt screen active, and the user is about to see
		// Go-side output.
		resetTerminal()
		return err
	}
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

	// A bare `rafiki attach` focuses nothing, so there is no single session to
	// offer to terminate. Prompting would ask about "" and killing would be a
	// fleet-wide action nobody asked for.
	if childID == "" {
		return nil
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

	// Use the same kill+close policy as `rafiki kill` so a confirmed
	// terminate also removes the child from `rafiki list` on clean exit.
	res, err := killAndMaybeClose(cmdCtx(cmd), c, childID, 0, 0, false)
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	if res.CloseErr != nil {
		fmt.Fprintf(os.Stderr, "warning: close after kill failed: %v\n", res.CloseErr)
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
	seen := make(map[string]struct{})
	var out []string
	for _, ch := range completionChildren(cmd) {
		for k, v := range ch.Labels {
			if strings.HasPrefix(k, "rafiki/") {
				continue
			}
			pair := k + "=" + v
			if _, ok := seen[pair]; ok {
				continue
			}
			seen[pair] = struct{}{}
			if strings.HasPrefix(pair, toComplete) {
				out = append(out, pair)
			}
		}
	}
	return out
}

// completeLabelKeys returns label key completions from all currently-known
// children, skipping rafiki/ auto-labels.  Used for --has-label flag completion.
func completeLabelKeys(cmd *cobra.Command, toComplete string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ch := range completionChildren(cmd) {
		for k := range ch.Labels {
			if strings.HasPrefix(k, "rafiki/") {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			if strings.HasPrefix(k, toComplete) {
				out = append(out, k)
			}
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
