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
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/tui"
)

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "attach [id|name]",
		Aliases: []string{"at"},
		Short:   "Attach the rafiki TUI to a running child",
		Long: `Attach a terminal UI to an existing child.

The TUI streams the conversation live over the Connect control plane, on the
daemon's local unix socket. It renders markdown, tool calls and thinking
blocks. Enter sends a prompt; Ctrl+C or Ctrl+D quits.

The child keeps running after you quit — reattach any time.`,
		Args: cobra.ExactArgs(1),
		RunE: runAttach,
	}
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips the exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips the exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	return cmd
}

func runAttach(cmd *cobra.Command, args []string) error {
	client, err := newControlClient(cmd)
	if err != nil {
		return err
	}
	childID, err := resolveChildConnect(cmdCtx(cmd), client, args[0])
	if err != nil {
		return err
	}
	return runTUIForChild(cmd, childID)
}

// resolveChildConnect maps an id or a name to a child id over Connect.
//
// Connect-only by design: the TUI must not straddle two transports. An input
// that already looks like a child id is returned as-is after a GetChild probe,
// which doubles as the capability pre-flight.
func resolveChildConnect(ctx context.Context, c rafikiv1connect.ControlClient, input string) (string, error) {
	if input == "" {
		return "", errors.New("no child specified; run `rafiki list` to see options")
	}

	if strings.HasPrefix(input, "c_") {
		_, err := c.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: input}))
		if err != nil {
			return "", diagnoseConnectError(err, paths.ConnectSocketPath())
		}
		return input, nil
	}

	resp, err := c.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return "", diagnoseConnectError(err, paths.ConnectSocketPath())
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
func runTUIForChild(cmd *cobra.Command, childID string) error {
	sock := paths.ConnectSocketPath()
	m := tui.NewModel(tui.Options{
		HTTPClient: connectHTTPClient(sock),
		BaseURL:    connectUDSBaseURL,
		ChildID:    childID,
	})
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
