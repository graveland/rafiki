// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net/http"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
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
	base := paths.Get(paths.URL)
	if base == "" {
		base = defaultControlURL
	}
	// RAFIKI_URL may name a REMOTE daemon's TLS control listener, which is a
	// different listener from the local proxy face where the Connect plane is
	// mounted — dialing it here fails with an opaque transport error rather
	// than saying what is wrong.
	if client.IsRemoteURL(base) {
		return fmt.Errorf("rafiki history does not support a remote control plane yet "+
			"(RAFIKI_URL=%s names one); this phase serves the Connect control plane over "+
			"the local proxy listener only", base)
	}
	controlClient := rafikiv1connect.NewControlClient(http.DefaultClient, base)

	req := connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: args[0]})
	// The Connect control plane is mounted under the same auth middleware as
	// every other HTTP face (see pkg/server/handler.go), so it accepts the
	// one bearer credential every rafiki client already resolves this way.
	if tok := paths.TokenFromEnv(); tok != "" {
		req.Header().Set("Authorization", "Bearer "+tok)
	}

	resp, err := controlClient.GetHistory(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("get history: %w", err)
	}
	renderHistory(cmd.OutOrStdout(), resp.Msg.Events)
	return nil
}

const defaultControlURL = "http://127.0.0.1:8035"
