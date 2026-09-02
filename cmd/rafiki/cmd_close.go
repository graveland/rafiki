package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

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
		Short:   "Stop (if needed) and finalize conversations",
		Long: `Finalize one or more children, stopping them first if still running.

A closed conversation can never be resumed, reattached or continued again: the
child leaves the controller's store and its conversations.child row is dropped.

If a target is still running, close stops it first (the same graceful,
escalate-to-SIGKILL sequence as 'rafiki stop') and then closes it —
unconditionally, regardless of how the stop went. Closing is explicit: unlike
'rafiki stop', which only auto-closed on a clean exit, asking to close means
get rid of it.

Its TRANSCRIPT is kept. No foreign key references conversations.child, so the
conversation stays fully readable through 'rafiki history' and
'rafiki conversations' after closing. What is reclaimed is the child's log dump
directory and, for fundi children, its clipped-output spill directory.

With --all-exited, closes every already-exited child (optionally filtered by
--older-than). --all-exited never stops a running child — only closing by
id|name does that.`,
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
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout when a target must be stopped first (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout when a target must be stopped first (e.g. 30s)")
	// Any child is a valid target now: close stops a live one first, so
	// completion is not restricted to already-exited children the way it was
	// before close learned to do that.
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
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
		dropChildCompletionCache(cmd)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	}

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")

	var failures int
	for _, arg := range args {
		childID, err := c.Resolve(ctx, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		if err := closeChild(ctx, c, childID, st, kt); err != nil {
			fmt.Fprintf(os.Stderr, "error: close %q: %v\n", arg, err)
			failures++
			continue
		}
		fmt.Printf("closed %s\n", childID)
	}
	dropChildCompletionCache(cmd)
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// closeChild kills childID if it is still running — ignoring the "already
// exited" case rather than treating it as failure — then closes it. Shared
// by `rafiki close` and the attach/create exit prompt's "terminate" choice,
// which is close's semantics under a different name: an explicit request to
// get rid of the child, not an implicit safety net, so this does NOT gate on
// a clean exit the way the old kill-then-auto-close policy did.
func closeChild(ctx context.Context, c *client.Client, childID string, st, kt time.Duration) error {
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

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		if resp.Error == nil || resp.Error.Code != protocol.ErrChildExited {
			return fmt.Errorf("ctrl_kill: %s", client.FormatError(resp))
		}
		// Already exited — proceed straight to close.
	}

	fresp, err := c.Request(ctx, protocol.ForgetRequest{
		Type:    protocol.TypeCtrlForget,
		ChildID: childID,
	})
	if err != nil {
		return err
	}
	if !fresp.Success {
		return fmt.Errorf("ctrl_forget: %s", client.FormatError(fresp))
	}
	return nil
}
