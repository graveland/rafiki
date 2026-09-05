package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/protocol"
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
		[]string{"spawning", "idle", "streaming", "tool_running", "compacting", "blocked_ui", "shutting_down", "exited"},
		cobra.ShellCompDirectiveNoFileComp,
	))

	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	filter := protocol.ListFilter{}
	if v, _ := cmd.Flags().GetString("status"); v != "" {
		filter.Status = v
	}
	if v, _ := cmd.Flags().GetString("name-contains"); v != "" {
		filter.NameContains = v
	}
	if v, _ := cmd.Flags().GetString("cwd-contains"); v != "" {
		filter.CwdContains = v
	}
	if labelPairs, _ := cmd.Flags().GetStringArray("label"); len(labelPairs) > 0 {
		labels, err := parseLabelPairs(labelPairs)
		if err != nil {
			return fmt.Errorf("--label: %w", err)
		}
		filter.Labels = labels
	}
	if hasLabels, _ := cmd.Flags().GetStringArray("has-label"); len(hasLabels) > 0 {
		filter.HasLabel = hasLabels
	}

	children, err := c.List(cmdCtx(cmd), filter)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	mode, useColor := outputOpts(cmd)
	if mode == outputTable {
		fmt.Fprint(os.Stdout, profileIndicator(mustProfile(cmd).Name))
	}
	flat, _ := cmd.Flags().GetBool("flat")
	return renderList(os.Stdout, children, mode, useColor, flat)
}
