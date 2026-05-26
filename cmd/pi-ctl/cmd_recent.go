package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newRecentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recent [id|name]",
		Short: "Show recent events from a child's ring buffer",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runRecent,
	}
	cmd.Flags().Int("limit", 100, "Maximum number of events")
	cmd.Flags().Duration("since", 0, "Only events newer than this (e.g. 5m)")
	cmd.Flags().StringSlice("include", nil, "Include only these event types (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude these event types (repeatable)")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runRecent(cmd *cobra.Command, args []string) error {
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

	limit, _ := cmd.Flags().GetInt("limit")
	since, _ := cmd.Flags().GetDuration("since")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")

	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ChildID: childID,
		Limit:   limit,
		Include: include,
		Exclude: exclude,
	}
	if since > 0 {
		req.Since = time.Now().Add(-since).UnixMilli()
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_get_recent: %s", client.FormatError(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
