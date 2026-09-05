// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui"
)

// attachReadsExitFlags records that runAttach honours --kill-on-exit and
// --keep-on-exit. Before C1b the flags were declared on `attach` and read by
// nobody -- `create` read both -- and the two tests covering them asserted only
// that they were DECLARED, which is how a flag that did nothing survived C0.
const attachReadsExitFlags = true

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "attach [id|name]",
		Aliases: []string{"at"},
		Short:   "Attach the rafiki cockpit to running children",
		Long: `Attach the cockpit — a tree rail beside a conversation.

With no argument it opens over every child you can see, with nothing focused:
pick one from the rail. With an id or name it opens focused on that child and
follows its delegation, so agents it spawns appear in the rail without
resubscribing.

⏎ sends a prompt — it steers the agent if it is mid-turn — ⇧⏎ (or ^J) inserts a
newline, ⌥⏎ (or ^S) steers the turn already running, esc (or ^X) aborts it.
^O expands full tool arguments and thinking, ^L repaints the screen.
⇥ cycles the three panes — input, agents, transcript — and each one owns its
keys while it has focus. ⌥N hops to the next agent that needs you, ^↑/^↓ move,
^B collapses the rail, ^G toggles help. Children keep running after you quit —
reattach any time.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runAttach,
	}
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the focused session when the cockpit quits (skips the exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep sessions running on exit (skips the exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, isAttachable), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

// isAttachable reports whether the cockpit can usefully focus on ch: it has
// to still be running, since there is nothing to steer or watch on a
// "spawning" child that hasn't produced a session yet or an "exited" one
// that has none left to attach to. "shutting_down" is excluded for the same
// reason exited is — the stream is closing, not something to attach into.
func isAttachable(ch completionChild) bool {
	switch protocol.Status(ch.Status) {
	case protocol.StatusIdle,
		protocol.StatusStreaming,
		protocol.StatusToolRunning,
		protocol.StatusCompacting,
		protocol.StatusBlockedUI:
		return true
	}
	return false
}

// subjectFor builds the rail subscription for an entry point.
//
// Bare attach is `all`. Attaching to a child is its SUBTREE PLUS ITSELF:
// eventlog.ScopeSubtree never includes the root, so without include_self the
// attached child's own rail row would freeze the moment you hop off it -- and
// the focus stream, being child-scoped, would hide that until you did.
//
// MaxDepth stays 0, which means UNLIMITED. A watcher wants a complete model;
// the agent-facing path is the one that sets 1.
func subjectFor(childID string) *rafikiv1.EventSubject {
	if childID == "" {
		return &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}}
	}
	return &rafikiv1.EventSubject{
		Scope:       &rafikiv1.EventSubject_Subtree{Subtree: childID},
		IncludeSelf: true,
	}
}

func runAttach(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()

	var childID string
	if len(args) == 1 {
		if childID, err = resolveChildConnect(cmdCtx(cmd), ep, client, args[0]); err != nil {
			return err
		}
	} else {
		// Rail-first. The capability pre-flight still has to happen BEFORE the
		// alt screen: a daemon that cannot serve the cockpit must produce a
		// line on stderr, not a working-looking UI that answers nothing.
		if _, err := client.ListChildren(cmdCtx(cmd),
			connect.NewRequest(&rafikiv1.ListChildrenRequest{})); err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	return attachAndDecide(cmd, ep, childID, killOnExit, keepOnExit)
}

// resolveChildConnect maps an id or a name to a child id over Connect.
//
// Connect-only by design: the TUI must not straddle two transports. An input
// that already looks like a child id is returned as-is after a GetChild probe,
// which doubles as the capability pre-flight.
func resolveChildConnect(ctx context.Context, ep connectEndpoint, c rafikiv1connect.ControlClient, input string) (string, error) {
	if input == "" {
		return "", errors.New("no child specified; run `rafiki list` to see options")
	}

	if strings.HasPrefix(input, "c_") {
		_, err := c.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: input}))
		if err != nil {
			return "", diagnoseConnectError(err, ep.describe)
		}
		return input, nil
	}

	resp, err := c.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return "", diagnoseConnectError(err, ep.describe)
	}
	var matches []string
	for _, ch := range resp.Msg.GetChildren() {
		if ch.GetName() == input || ch.GetChildId() == input {
			matches = append(matches, ch.GetChildId())
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no child named %q; run `rafiki list` to see options", input)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q matches %d children; use the child id instead", input, len(matches))
	}
}

// runTUIForChild runs the bubbletea program for one child.
//
// The capability pre-flight already ran in resolveChildConnect, BEFORE the alt
// screen is entered. That ordering is the point: a daemon that cannot serve
// the TUI must produce a line on stderr, not a working-looking UI that answers
// nothing. The predecessor logged a warning and continued.
func runTUIForChild(cmd *cobra.Command, ep connectEndpoint, childID string) error {
	// Capture the process's own logs while the alt screen is up. Without this
	// the session executor's join/park/reconnect messages go to the default
	// handler -- stderr -- and corrupt the cockpit mid-draw.
	ring, restoreLogging, err := installTUILogging(500)
	if err != nil {
		return err
	}
	defer restoreLogging()
	m := tui.NewCockpit(tui.Options{
		HTTPClient:  ep.httpClient,
		BaseURL:     ep.baseURL,
		ChildID:     childID,
		Subject:     subjectFor(childID),
		ProfileName: mustProfile(cmd).Name,
	})
	// Run returns the final model too; the cockpit holds all state worth
	// keeping, so it is discarded.
	_, runErr := tea.NewProgram(m).Run()
	if runErr != nil {
		// After the alt screen is gone, not before. A dying executor that
		// logged nowhere is worse than a corrupted screen.
		ring.Dump()
		return fmt.Errorf("tui: %w", runErr)
	}
	return nil
}
