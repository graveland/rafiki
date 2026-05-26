package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newKillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill [id|name]",
		Short: "Stop a running child gracefully",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runKill,
	}
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout (e.g. 30s)")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
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

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")

	req := protocol.KillRequest{
		Type:    protocol.TypeCtrlKill,
		ChildID: childID,
	}
	if st > 0 {
		req.ShutdownTimeoutMs = st.Milliseconds()
	}
	if kt > 0 {
		req.KillTimeoutMs = kt.Milliseconds()
	}

	// Allow longer than Go's default RPC timing if both flags push past it.
	total := st + kt
	if total > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total+5*time.Second)
		defer cancel()
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_kill: %s", client.FormatError(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
