package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
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

A target with subagents is refused until you say what to do with them:
--include-subagents=true stops and closes them too (deepest first, before the
target), --include-subagents=false closes only the target.

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
	addIncludeSubagentsFlag(cmd, "close")
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
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

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
		req := &rafikiv1.CloseAllExitedRequest{}
		if olderThan > 0 {
			req.OlderThan = durationpb.New(olderThan)
		}
		resp, err := ctrl.CloseAllExited(ctx, connect.NewRequest(req))
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		dropChildCompletionCache(cmd)
		if review {
			closeReview(cmd, resp.Msg.GetChildIds())
		}
		return renderCloseAllExited(os.Stdout, resp.Msg, mode)
	}

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")
	profileName := mustProfile(cmd).Name

	var failures int
	for _, arg := range args {
		childID, err := resolveTargetConnect(ctx, ctrl, profileName, arg, ep.describe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		include, err := includeSubagents(ctx, cmd, ctrl, childID, arg, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: close %q: %v\n", arg, err)
			failures++
			continue
		}
		if err := closeChildConnect(ctx, ctrl, childID, include, st, kt); err != nil {
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
// Connect. Best-effort by design (design §5): the close has already succeeded,
// so a review that fails must never fail it — every whole-request failure
// becomes one stderr note per closed id, and a per-id
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

// renderCloseAllExited writes the CloseAllExited result in the requested
// mode. JSON is the response's canonical protojson ({"childIds":[...]}); JSONL
// writes one closed child id per line; text reports the count. The per-target
// `closed <id>` path lives in runClose and is deliberately untouched here.
func renderCloseAllExited(w io.Writer, resp *rafikiv1.CloseAllExitedResponse, mode outputMode) error {
	switch mode {
	case outputJSON:
		return emitProto(w, resp, mode)
	case outputJSONL:
		rows := make([]any, 0, len(resp.GetChildIds()))
		for _, id := range resp.GetChildIds() {
			rows = append(rows, id)
		}
		return writeJSONL(w, rows)
	default:
		if len(resp.GetChildIds()) == 0 {
			_, err := fmt.Fprintln(w, "no exited children to close")
			return err
		}
		_, err := fmt.Fprintf(w, "closed %d exited children\n", len(resp.GetChildIds()))
		return err
	}
}

// closeChildConnect kills childID if it is still running — treating "already
// exited" as nothing to stop, never as failure — then closes it. It is
// close's semantics under the Connect plane: an explicit request to get rid
// of the child, not an implicit safety net, so this does NOT gate on a clean
// exit the way the old kill-then-auto-close policy did.
//
// Already-exited is detected twice, because the signal is now the rafiki
// reason on the Connect error (rpcreason.Reason — NEVER connect.CodeOf, whose
// FailedPrecondition also carries child_in_grace/child_shutting_down):
//
//   - A GetChild pre-check skips the kill entirely for a child the daemon
//     already reports exited, so the ordinary `close <exited-child>` needs
//     neither a kill nor a reason. (It also saves a graceful-shutdown wait
//     the old flow always paid.)
//   - The kill error's own ErrChildExited reason covers the race between the
//     pre-check and the kill. A kill failure carrying no reason is reported
//     as the failure it is.
func closeChildConnect(ctx context.Context, ctrl rafikiv1connect.ControlClient, childID string, include bool, st, kt time.Duration) error {
	get, err := ctrl.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		return fmt.Errorf("get child: %s", formatConnectErr(err))
	}
	// With include, the kill runs even for an exited target: its subagents may
	// still be live, and the cascade ends them before the target is touched.
	if include || get.Msg.GetChild().GetStatus() != string(protocol.StatusExited) {
		req := &rafikiv1.KillRequest{ChildId: childID, IncludeDescendants: include}
		if st > 0 {
			req.ShutdownTimeout = durationpb.New(st)
		}
		if kt > 0 {
			req.KillTimeout = durationpb.New(kt)
		}
		_, err := ctrl.Kill(ctx, connect.NewRequest(req))
		if err != nil {
			if rpcreason.Reason(err) != protocol.ErrChildExited {
				return fmt.Errorf("kill: %s", formatConnectErr(err))
			}
			// Already exited — proceed straight to close.
		}
	}

	_, err = ctrl.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{ChildId: childID, IncludeDescendants: include}))
	if err != nil {
		return fmt.Errorf("close: %s", formatConnectErr(err))
	}
	return nil
}
