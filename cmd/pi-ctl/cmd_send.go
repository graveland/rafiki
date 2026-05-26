package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <id|name> [frame-json]",
		Short: "Send a raw pi-RPC frame to a child",
		Long: `Send a raw pi-RPC frame (debugging or scripting).

If frame-json is omitted, read the frame from stdin.

Example:
  pi-ctl send afk-impl '{"type":"prompt","message":"Hello!"}'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: runSend,
	}
}

func runSend(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
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
