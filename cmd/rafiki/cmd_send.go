package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "send [id|name] [frame-json]",
		Aliases: []string{"snd"},
		Short:   "Send a raw pi-RPC frame to a child",
		Long: `Send a raw pi-RPC frame (debugging or scripting).

If id|name is omitted, uses the active marker. If frame-json is omitted, reads from stdin.

Example:
  rafiki send afk-impl '{"type":"prompt","message":"Hello!"}'`,
		Args: cobra.RangeArgs(0, 2),
		RunE: runSend,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runSend(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTarget(ctx, c, input)
	if err != nil {
		return err
	}

	var frame json.RawMessage
	if len(args) == 2 {
		frame = json.RawMessage(args[1])
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		frame = json.RawMessage(b)
	}

	// Validate it parses before sending.
	var probe map[string]any
	if err := json.Unmarshal(frame, &probe); err != nil {
		return fmt.Errorf("frame is not valid JSON: %w", err)
	}

	resp, err := c.Request(ctx, protocol.SendRequest{
		Type:    protocol.TypeCtrlSend,
		ChildID: childID,
		Frame:   frame,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_send: %s", client.FormatError(resp))
	}
	return nil
}
