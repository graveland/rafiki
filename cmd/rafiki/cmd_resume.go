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
		Use:     "resume [id|name]",
		Aliases: []string{"res"},
		Short:   "Resume an exited child",
		Long: `Resume a rafiki-managed child that has exited.

  rafiki resume [id|name]

If id|name is omitted, uses the active marker.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	_ = cmd.RegisterFlagCompletionFunc("api-key", cobra.NoFileCompletions)

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
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
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTargetConnect(ctx, ctrl, p.Name, input)
	if err != nil {
		return err
	}

	apiKey, _ := cmd.Flags().GetString("api-key")

	resp, err := ctrl.Resume(ctx, connect.NewRequest(&rafikiv1.ResumeRequest{
		ChildId: childID,
		ApiKey:  apiKey,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	_ = setActive(p.Name, childID)

	// The Resume response carries the child id only; text mode fetches a name
	// with a best-effort get. A JSON/JSONL consumer does not need it and pays
	// no extra round trip.
	name := ""
	if mode == outputTable {
		name = resumeChildName(ctx, ctrl, childID)
	}
	return renderResume(os.Stdout, childID, name, resp.Msg, mode)
}

// renderResume writes the resume result in the requested mode: the Resume
// response's canonical protojson pretty, one compact line, or the text line
// `resumed <childID> (<name>)` — the name is dropped, never printed empty,
// when the follow-up get could not resolve it.
func renderResume(w io.Writer, childID, name string, msg proto.Message, mode outputMode) error {
	switch mode {
	case outputJSON, outputJSONL:
		return emitProto(w, msg, mode)
	default:
		line := "resumed " + childID
		if name != "" {
			line += " (" + name + ")"
		}
		_, err := fmt.Fprintln(w, line)
		return err
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
