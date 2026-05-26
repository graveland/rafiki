package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active and recently-exited children",
		Args:  cobra.NoArgs,
		RunE:  runList,
	}
	cmd.Flags().String("status", "", "Filter by status (e.g. idle, streaming, exited)")
	cmd.Flags().String("name-contains", "", "Filter by substring in name")
	cmd.Flags().String("cwd-contains", "", "Filter by substring in working directory")

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

	children, err := c.List(cmdCtx(cmd), filter)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	mode, useColor := outputOpts(cmd)
	return renderList(os.Stdout, children, mode, useColor)
}
