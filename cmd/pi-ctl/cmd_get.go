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
	cmd := &cobra.Command{
		Use:   "get [id|name]",
		Short: "Show details for a single child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runGet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
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
