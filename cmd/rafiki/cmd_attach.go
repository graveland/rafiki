package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "attach [id|name]",
		Aliases: []string{"a"},
		Short:   "Attach the pi TUI to an existing child",
		Long: `Open the pi TUI driving an existing daemon-managed child.

When the TUI quits (Ctrl+D, /quit), rafiki asks whether to terminate the session
or leave it running. Use --kill-on-exit or --keep-on-exit to skip the prompt
and choose explicitly.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runAttach,
	}
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.Flags().IntP("tail", "n", 500, "Scrollback: replay the last N retained events into the TUI (-1 = all, 0 = none)")
	cmd.Flags().Bool("no-local-executor", false,
		"do not offer this machine as a workspace; nothing here joins the daemon's executor pool for this session")
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

	// Offer this machine as a workspace for the attached session, unless the
	// operator opted out. The selector is unused — a child's executor is fixed
	// at spawn — so only the stop function is kept, to tear the executor down
	// when the TUI exits.
	noLocalExecutor, _ := cmd.Flags().GetBool("no-local-executor")
	if !noLocalExecutor {
		root := childCwd(cmdCtx(cmd), c, childID)
		if root == "" {
			root, _ = os.Getwd()
		}
		_, stop, err := startSessionExecutor(cmdCtx(cmd), c, root)
		if err != nil {
			return fmt.Errorf("this machine could not join as a workspace: %w", err)
		}
		defer stop()
	}

	tailN, _ := cmd.Flags().GetInt("tail")
	os.Setenv(paths.AttachTail, strconv.Itoa(tailN))

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	return attachAndDecide(cmd, childID, killOnExit, keepOnExit)
}
