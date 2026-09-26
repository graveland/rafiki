package main

import (
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get [id|name...]",
		Short:   "Show details for one or more children",
		Aliases: []string{"show", "info"},
		Args:    cobra.MinimumNArgs(1),
		RunE:    runGet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

	ctx := cmdCtx(cmd)
	profileName := mustProfile(cmd).Name

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	var children []*rafikiv1.ChildSummary
	var failures int
	for _, arg := range args {
		childID, err := resolveTargetConnect(ctx, ctrl, profileName, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		resp, err := ctrl.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: get %q: %s\n", arg, formatConnectErr(err))
			failures++
			continue
		}
		children = append(children, resp.Msg.GetChild())
	}

	if err := emitGet(os.Stdout, args, children, failures, mode, useColor); err != nil {
		return err
	}

	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// emitGet writes get's output in the resolved mode. Every per-target failure
// has already gone to stderr by the time this runs, so all three modes emit
// the successful children only — data is never withheld because a sibling
// target failed.
//
// The JSON shapes are get's backward-compatibility contract and are preserved
// exactly (protojson-encoded now): a single successful target emits a bare
// ChildSummary object, multiple targets (or any failures) wrap in
// {"children":[...]}. JSONL emits one canonical ChildSummary per line,
// unwrapped. Table mode renders the same table `list` renders, flat.
func emitGet(w io.Writer, args []string, children []*rafikiv1.ChildSummary, failures int, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		// Single successful target: plain object for backward compatibility.
		// Multiple targets (or any failures): wrap in {"children":[...]}.
		if len(args) == 1 && failures == 0 && len(children) == 1 {
			return emitProto(w, children[0], outputJSON)
		}
		return writeProtoChildren(w, children)
	case outputJSONL:
		return emitProtoRows(w, children, outputJSONL)
	default:
		return renderList(w, children, outputTable, useColor, true)
	}
}
