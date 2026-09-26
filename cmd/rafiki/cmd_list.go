package main

import (
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active and recently-exited children",
		Args:    cobra.NoArgs,
		RunE:    runList,
	}
	cmd.Flags().String("status", "", "Filter by status (e.g. idle, streaming, exited)")
	cmd.Flags().String("name-contains", "", "Filter by substring in name")
	cmd.Flags().String("cwd-contains", "", "Filter by substring in working directory")
	cmd.Flags().StringArray("label", nil, "AND-match label k=v (repeatable)")
	cmd.Flags().StringArray("has-label", nil, "Filter children that have this label key (repeatable)")
	cmd.Flags().Bool("flat", false, "Render a flat list instead of a tree")

	_ = cmd.RegisterFlagCompletionFunc("status", cobra.FixedCompletions(
		[]string{"spawning", "idle", "streaming", "tool_running", "compacting", "batch_wait", "blocked_ui", "shutting_down", "exited"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelPairs(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("has-label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelKeys(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	req := &rafikiv1.ListChildrenRequest{}
	if v, _ := cmd.Flags().GetString("status"); v != "" {
		// The wire filter is a plural OR-match; the flag is one status.
		req.Statuses = []string{v}
	}
	if v, _ := cmd.Flags().GetString("name-contains"); v != "" {
		req.NameContains = v
	}
	if v, _ := cmd.Flags().GetString("cwd-contains"); v != "" {
		req.CwdContains = v
	}
	if labelPairs, _ := cmd.Flags().GetStringArray("label"); len(labelPairs) > 0 {
		labels, err := parseLabelPairs(labelPairs)
		if err != nil {
			return fmt.Errorf("--label: %w", err)
		}
		req.Labels = labels
	}
	if hasLabels, _ := cmd.Flags().GetStringArray("has-label"); len(hasLabels) > 0 {
		req.HasLabel = hasLabels
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().ListChildren(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	if mode == outputTable {
		fmt.Fprint(os.Stdout, profileIndicator(mustProfile(cmd).Name))
	}
	flat, _ := cmd.Flags().GetBool("flat")
	return renderList(os.Stdout, resp.Msg.GetChildren(), mode, useColor, flat)
}
