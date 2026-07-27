package main

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"git.graveland.dev/brent/fundi/protocol"
)

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach [id|name]",
		Short: "Attach the pi TUI to an existing child",
		Long: `Open the pi TUI driving an existing daemon-managed child.

When the TUI quits (Ctrl+D, /quit), fundi asks whether to terminate the session
or leave it running. Use --kill-on-exit or --keep-on-exit to skip the prompt
and choose explicitly.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runAttach,
	}
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.Flags().IntP("tail", "n", 500, "Scrollback: replay the last N retained events into the TUI (-1 = all, 0 = none)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	// Attachable: any live state except spawning/shutting_down.
	attachable := func(ch protocol.ChildSummary) bool {
		switch ch.Status {
		case string(protocol.StatusIdle),
			string(protocol.StatusStreaming),
			string(protocol.StatusToolRunning),
			string(protocol.StatusCompacting),
			string(protocol.StatusBlockedUI):
			return true
		}
		return false
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildrenByState(cmd, toComplete, attachable), cobra.ShellCompDirectiveNoFileComp
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

	tailN, _ := cmd.Flags().GetInt("tail")
	os.Setenv("PIC_ATTACH_TAIL", strconv.Itoa(tailN))

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	return attachAndDecide(cmd, childID, killOnExit, keepOnExit)
}
