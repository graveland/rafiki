package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget [id|name]",
		Short: "Drop an exited child from the controller",
		Long: `Drop an exited child from the controller's in-memory store.
Disk artifacts (logs, state record) are NOT removed.

With --all-exited, forgets every exited child (optionally filtered by --older-than).`,
		Args: cobra.MaximumNArgs(1),
		RunE: runForget,
	}
	cmd.Flags().Bool("all-exited", false, "Forget all exited children")
	cmd.Flags().Duration("older-than", 0, "Only forget exited children older than this")
	return cmd
}

func runForget(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	allExited, _ := cmd.Flags().GetBool("all-exited")

	if allExited {
		olderThan, _ := cmd.Flags().GetDuration("older-than")
		req := protocol.ForgetAllExitedRequest{
			Type: protocol.TypeCtrlForgetAllExited,
		}
		if olderThan > 0 {
			req.OlderThanMs = olderThan.Milliseconds()
		}
		resp, err := c.Request(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("ctrl_forget_all_exited: %s", client.FormatError(resp))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	}

	if len(args) == 0 {
		return fmt.Errorf("forget requires <id|name> or --all-exited")
	}

	childID, err := c.Resolve(ctx, args[0])
	if err != nil {
		return err
	}

	resp, err := c.Request(ctx, protocol.ForgetRequest{
		Type:    protocol.TypeCtrlForget,
		ChildID: childID,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_forget: %s", client.FormatError(resp))
	}
	fmt.Fprintln(os.Stderr, "forgot", childID)
	return nil
}
