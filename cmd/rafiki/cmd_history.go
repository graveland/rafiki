// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// renderHistory writes durable-tier events as plain text. It is separated from
// the command so it can be tested without a server.
func renderHistory(w io.Writer, evs []*rafikiv1.Event) {
	for _, ev := range evs {
		var role string
		var blocks []*rafikiv1.ContentBlock
		switch {
		case ev.GetAssistantMessage() != nil:
			role, blocks = "assistant", ev.GetAssistantMessage().Content
		case ev.GetUserMessage() != nil:
			role, blocks = "user", ev.GetUserMessage().Content
		default:
			continue
		}
		fmt.Fprintf(w, "[%d] %s\n", ev.GetOrdinal(), role)
		for _, b := range blocks {
			switch {
			case b.GetText() != nil:
				fmt.Fprintf(w, "  %s\n", b.GetText().Text)
			case b.GetThinking() != nil:
				fmt.Fprintf(w, "  (thinking) %s\n", b.GetThinking().Thinking)
			case b.GetToolUse() != nil:
				fmt.Fprintf(w, "  (tool %s) %s\n", b.GetToolUse().Name, b.GetToolUse().InputJson)
			case b.GetToolResult() != nil:
				fmt.Fprintf(w, "  (result for %s)\n", b.GetToolResult().ToolUseId)
			}
		}
	}
}

func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <childId>",
		Short: "Print a fundi child's conversation history over the Connect API",
		Args:  cobra.ExactArgs(1),
		RunE:  runHistory,
	}
}

func runHistory(cmd *cobra.Command, args []string) error {
	// newConnectEndpoint is the same resolver every other Connect-based
	// command (attach, tui) uses: it correctly handles both socket profiles
	// (dials the profile's own Connect UDS) and URL profiles (dials the
	// remote TLS endpoint), with the bearer credential attached via
	// bearerTransport. Building the endpoint here by hand (reading p.URL
	// directly and falling back to a hardcoded :8035) meant a socket
	// profile's `history` silently queried whatever daemon happened to be
	// listening on :8035 instead of the profile's own daemon.
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	req := connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: args[0]})
	resp, err := ep.control().GetHistory(cmd.Context(), req)
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	renderHistory(cmd.OutOrStdout(), resp.Msg.Events)
	return nil
}
