package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id|name>",
		Short: "Show details for a single child",
		Args:  cobra.ExactArgs(1),
		RunE:  runGet,
	}
}

func runGet(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil {
		return err
	}

	resp, err := c.Request(ctx, protocol.GetRequest{
		Type:    protocol.TypeCtrlGet,
		ChildID: childID,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_get: %s", client.FormatError(resp))
	}

	var child protocol.ChildSummary
	if err := json.Unmarshal(resp.Data, &child); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(child)
}
