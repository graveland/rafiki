package main

import (
	"github.com/spf13/cobra"
)

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach [id|name]",
		Short: "Attach the pi TUI to an existing child",
		Long: `Open the pi TUI driving an existing daemon-managed child.

Quitting the TUI (Ctrl+D, /quit) detaches by default — the session keeps
running. Use --kill-on-exit for native pi exit semantics.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runAttach,
	}
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits")
	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runAttach(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(cmdCtx(cmd), c, target)
	if err != nil {
		return err
	}
	if err := setActive(childID); err != nil {
		// Best effort.
		_ = err
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	return execPicAttach(childID, killOnExit)
}
