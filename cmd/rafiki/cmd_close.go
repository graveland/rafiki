package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "close [id|name...]",
		// `forget` is kept forever, not deprecated: it is in muscle memory and
		// in scripts, and an alias costs one line.
		Aliases: []string{"forget", "rm"},
		Short:   "Finalize exited conversations",
		Long: `Finalize one or more exited children.

A closed conversation can never be resumed, reattached or continued again: the
child leaves the controller's store and its conversations.child row is dropped.

Its TRANSCRIPT is kept. No foreign key references conversations.child, so the
conversation stays fully readable through 'rafiki history' and
'rafiki conversations' after closing. What is reclaimed is the child's log dump
directory and, for fundi children, its clipped-output spill directory.

With --all-exited, closes every exited child (optionally filtered by
--older-than).`,
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
		RunE: runClose,
	}
	cmd.Flags().Bool("all-exited", false, "Close all exited children")
	cmd.Flags().Duration("older-than", 0, "Only close exited children older than this")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, func(ch completionChild) bool {
			return ch.Status == string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runClose(cmd *cobra.Command, args []string) error {
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
		dropChildCompletionCache()
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
			fmt.Fprintf(os.Stderr, "error: close %q: %v\n", arg, err)
			failures++
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "error: close %q: %s\n", arg, client.FormatError(resp))
			failures++
			continue
		}
		fmt.Printf("closed %s\n", childID)
	}
	dropChildCompletionCache()
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}
