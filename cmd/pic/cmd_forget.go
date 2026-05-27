package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/client"
	"graveland.dev/pi-controller/protocol"
)

func newForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget [id|name...]",
		Short: "Drop exited children from the controller",
		Long: `Drop one or more exited children from the controller's in-memory store.
Disk artifacts (logs, state record) are NOT removed.

With --all-exited, forgets every exited child (optionally filtered by --older-than).`,
		Args: func(cmd *cobra.Command, args []string) error {
			allExited, _ := cmd.Flags().GetBool("all-exited")
			if allExited {
				return nil // --all-exited ignores positional args
			}
			if len(args) == 0 {
				return fmt.Errorf("at least one id|name required (or use --all-exited)")
			}
			return nil
		},
		RunE: runForget,
	}
	cmd.Flags().Bool("all-exited", false, "Forget all exited children")
	cmd.Flags().Duration("older-than", 0, "Only forget exited children older than this")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, func(ch protocol.ChildSummary) bool {
			return ch.Status == string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
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

	var failures int
	for _, arg := range args {
		childID, err := c.Resolve(ctx, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		resp, err := c.Request(ctx, protocol.ForgetRequest{
			Type:    protocol.TypeCtrlForget,
			ChildID: childID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: forget %q: %v\n", arg, err)
			failures++
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "error: forget %q: %s\n", arg, client.FormatError(resp))
			failures++
			continue
		}
		fmt.Printf("forgot %s\n", childID)
	}
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}
