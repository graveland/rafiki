// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "send [id|name] [frame-json]",
		Aliases: []string{"snd"},
		Short:   "Send a raw child-protocol frame to a live child",
		Long: `Send a raw child-protocol frame to a child's stdin (debugging or scripting).

If id|name is omitted, uses the active marker. If frame-json is omitted, reads from stdin.
The frame is validated as JSON locally, before anything is sent.

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
	ctx := cmdCtx(cmd)

	// The frame comes first, so an invalid one is refused before anything is
	// dialled: the JSON gate is local, and the daemon never sees a frame the
	// CLI itself would reject.
	var frame string
	if len(args) == 2 {
		frame = args[1]
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		frame = string(b)
	}
	// Validate it parses — as an object — before sending. The daemon's
	// SendFrame refuses anything but a JSON object too; refusing here first
	// keeps a typo off the wire.
	var probe map[string]any
	if err := json.Unmarshal([]byte(frame), &probe); err != nil {
		return fmt.Errorf("frame is not valid JSON: %w", err)
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTargetConnect(ctx, client, mustProfile(cmd).Name, target)
	if err != nil {
		return err
	}

	if _, err := client.SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: frame,
	})); err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	return nil
}
