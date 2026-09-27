package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "resume [id|name...]",
		Aliases: []string{"res"},
		Short:   "Resume one or more exited children",
		Long: `Resume one or more rafiki-managed children that have exited.

  rafiki resume [id|name...]

If no id|name is given, uses the active marker.`,
		Args: cobra.ArbitraryArgs,
		RunE: runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	_ = cmd.RegisterFlagCompletionFunc("api-key", cobra.NoFileCompletions)

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, func(ch completionChild) bool {
			return ch.Status == string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runResume(cmd *cobra.Command, args []string) error {
	// Resolve the mode before dialing: -j and -J together is a user-input
	// error and must not cost a connection.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

	ctx := cmdCtx(cmd)
	p := mustProfile(cmd)
	apiKey, _ := cmd.Flags().GetString("api-key")

	// No args resumes the active marker, same as before the command grew
	// multi-target support: resolveTargetConnect treats "" as "use active".
	targets := args
	if len(targets) == 0 {
		targets = []string{""}
	}

	var results []resumeResult
	var failures int
	for _, arg := range targets {
		childID, err := resolveTargetConnect(ctx, ctrl, p.Name, arg, ep.describe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}

		resp, err := ctrl.Resume(ctx, connect.NewRequest(&rafikiv1.ResumeRequest{
			ChildId: childID,
			ApiKey:  apiKey,
		}))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resume %q: %s\n", arg, formatConnectErr(err))
			failures++
			continue
		}

		_ = setActive(p.Name, childID)

		// The Resume response carries the child id only; text mode fetches a
		// name with a best-effort get. A JSON/JSONL consumer does not need it
		// and pays no extra round trip.
		name := ""
		if mode == outputTable {
			name = resumeChildName(ctx, ctrl, childID)
		}
		results = append(results, resumeResult{ChildID: childID, Name: name, Resp: resp.Msg})
	}

	if err := renderResume(os.Stdout, len(targets), results, mode); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// resumeResult is one successfully resumed target. Failed targets never
// reach here — their error is reported to stderr as it happens, mirroring
// `rafiki get`'s multi-target error handling.
type resumeResult struct {
	ChildID string
	Name    string
	Resp    *rafikiv1.ResumeResponse
}

// renderResume writes the successful resume results in the requested mode.
// wanted is the number of targets requested (before failures are dropped): a
// single requested target that succeeded emits the bare Resume response's
// canonical protojson, preserving the single-target JSON/JSONL contract from
// before multi-target support; anything else wraps in the `{"rows":[...]}`
// envelope shared by the CLI's other multi-target verbs. Table mode prints
// one `resumed <childID> (<name>)` line per success, name dropped when the
// follow-up get could not resolve it.
func renderResume(w io.Writer, wanted int, results []resumeResult, mode outputMode) error {
	switch mode {
	case outputJSON, outputJSONL:
		if wanted == 1 && len(results) == 1 {
			return emitProto(w, results[0].Resp, mode)
		}
		msgs := make([]proto.Message, len(results))
		for i, r := range results {
			msgs[i] = r.Resp
		}
		return emitProtoRows(w, msgs, mode)
	default:
		for _, r := range results {
			line := "resumed " + r.ChildID
			if r.Name != "" {
				line += " (" + r.Name + ")"
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		return nil
	}
}

// resumeChildName fetches the child's name for text output, best effort: a
// failed lookup degrades to a bare `resumed <childID>` rather than failing a
// resume that succeeded.
func resumeChildName(ctx context.Context, ctrl rafikiv1connect.ControlClient, childID string) string {
	resp, err := ctrl.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		return ""
	}
	return resp.Msg.GetChild().GetName()
}
