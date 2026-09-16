package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
id|name does that.

With --review, each closed conversation is also handed to the daemon's
conversation review (over Connect, after the close). The close has already
succeeded at that point, so a review that fails — a whole-request error or a
per-id ALREADY_RUNNING/QUEUE_FULL — only prints a note; it never fails the
close. --no-review is the explicit no-op spelling of the default, for scripts
that pass a fixed flag set.`,
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
	cmd.Flags().Bool("review", false, "After closing, ask the daemon to review each closed conversation (failure notes only — never fails the close)")
	// Deliberately no mutual-exclusion validation: --no-review is the explicit
	// no-op spelling of the default (design §5), so a script can pass a fixed
	// flag set whether or not something else added --review.
	cmd.Flags().Bool("no-review", false, "Explicitly skip the post-close review (the default)")
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
	// Only --review is read: --no-review is a no-op spelling of the default,
	// so there is nothing to combine and no custom pair validation to add.
	review, _ := cmd.Flags().GetBool("review")

	if allExited {
		// Resolve the mode before sending the request: -j and -J together is a
		// user-input error and must not close anything first.
		mode, _, err := outputOpts(cmd)
		if err != nil {
			return err
		}
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
		if review {
			var data protocol.ForgetAllExitedResponseData
			if err := json.Unmarshal(resp.Data, &data); err != nil {
				// No ids to name without the decode; one unprefixed note.
				fmt.Fprintf(os.Stderr, "review: decode close response: %v\n", err)
			} else {
				closeReview(cmd, data.Children)
			}
		}
		return renderCloseAllExited(os.Stdout, json.RawMessage(resp.Data), mode)
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
		if review {
			closeReview(cmd, []string{childID})
		}
	}
	dropChildCompletionCache(cmd)
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// closeReview asks the daemon to review the conversations just closed, over
// Connect — the close ran on the framed protocol and this is a new verb, so
// it goes on Connect only. Best-effort by design (design §5): the close has
// already succeeded, so a review that fails must never fail it — every
// whole-request failure becomes one stderr note per closed id, and a per-id
// ALREADY_RUNNING/QUEUE_FULL becomes the id's own note. Enqueued prints
// nothing: silence is the success case here, the same way the close's own
// `closed <id>` line is the only close output.
func closeReview(cmd *cobra.Command, ids []string) {
	if len(ids) == 0 {
		return
	}
	note := func(err error) {
		for _, id := range ids {
			fmt.Fprintf(os.Stderr, "review %s: %s\n", id, err)
		}
	}

	cfg, err := loadReviewConfig()
	if err != nil {
		note(err)
		return
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		note(err)
		return
	}
	req := &rafikiv1.ConversationReviewRequest{ConversationIds: ids}
	// Stage stays unspecified: the wire enum's zero value defaults to detect
	// daemon-side, which is the stage close wants — rank persists findings
	// and belongs to `rafiki conversations review --stage rank`.
	cfg.mergeInto(req)

	resp, err := ep.control().ConversationReview(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		note(diagnoseConnectError(err, ep.describe))
		return
	}
	for _, acc := range resp.Msg.GetAccepted() {
		if s := acc.GetStatus(); s != rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED {
			fmt.Fprintf(os.Stderr, "review %s: %s\n", acc.GetConversationId(), reviewAcceptStatusText(s))
		}
	}
}

// renderCloseAllExited writes the ctrl_forget_all_exited result in the
// requested mode. JSON stays the raw payload passthrough it has always been;
// JSONL writes one closed child id per line (the response's children list);
// text reports the count. The per-target `closed <id>` path lives in runClose
// and is deliberately untouched here.
func renderCloseAllExited(w io.Writer, raw json.RawMessage, mode outputMode) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(raw)
	case outputJSONL:
		var data protocol.ForgetAllExitedResponseData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("decode close response: %w", err)
		}
		rows := make([]any, 0, len(data.Children))
		for _, id := range data.Children {
			rows = append(rows, id)
		}
		return writeJSONL(w, rows)
	default:
		var data protocol.ForgetAllExitedResponseData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("decode close response: %w", err)
		}
		if data.Count == 0 {
			_, err := fmt.Fprintln(w, "no exited children to close")
			return err
		}
		_, err := fmt.Fprintf(w, "closed %d exited children\n", data.Count)
		return err
	}
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
