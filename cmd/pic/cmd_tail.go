package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail [id|name]",
		Short: "Stream events from a child",
		Long: `Subscribe to a child's event stream and render events as they arrive.

By default, token-by-token message_update deltas are suppressed (--no-deltas=true).
Use --profile to select a named filter preset, or --include/--exclude to customize.

pic tail exits automatically when the child exits.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runTail,
	}
	cmd.Flags().String("profile", "", "Subscription profile: firehose|results|coarse|lifecycle")
	cmd.Flags().StringSlice("include", nil, "Add event types to subscription (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude event types from subscription (repeatable)")
	cmd.Flags().Bool("no-deltas", true, "Suppress token-by-token message_update deltas (default true)")
	cmd.Flags().BoolP("verbose", "v", false, "Include internal RPC response frames (autocomplete fetches, get_state, etc.)")

	_ = cmd.RegisterFlagCompletionFunc("profile", cobra.FixedCompletions(
		[]string{"firehose", "results", "coarse", "lifecycle"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("include", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("exclude", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
	})

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runTail(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx, cancel := context.WithCancel(cmdCtx(cmd))
	defer cancel()

	// SIGINT/SIGTERM cancel the context so the render loop exits cleanly.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(ctx, c, target)
	if err != nil {
		return err
	}
	// Best-effort: update the active marker so subsequent no-arg commands
	// default to this child.
	_ = setActive(childID)

	events, cancelSub, err := c.Subscribe()
	if err != nil {
		return err
	}
	defer cancelSub()

	profile, _ := cmd.Flags().GetString("profile")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")
	noDeltas, _ := cmd.Flags().GetBool("no-deltas")
	if noDeltas {
		exclude = append(exclude, "message_update")
	}

	subReq := protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ChildID: childID,
	}
	if profile != "" || len(include) > 0 || len(exclude) > 0 {
		subReq.Filter = &protocol.SubscribeFilter{
			Profile: profile,
			Include: include,
			Exclude: exclude,
		}
	}

	resp, err := c.Request(ctx, subReq)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_subscribe: %s", client.FormatError(resp))
	}

	mode, useColor := outputOpts(cmd)
	verbose, _ := cmd.Flags().GetBool("verbose")
	renderer := newTailRenderer(os.Stdout, useColor, mode, verbose)

	for {
		select {
		case frame, ok := <-events:
			if !ok {
				return nil
			}
			if err := renderer.render(frame); err != nil {
				fmt.Fprintln(os.Stderr, "render error:", err)
			}
			if isChildExited(frame, childID) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// isChildExited returns true if the frame is a ctrl_child_exited event
// for the given childId.
func isChildExited(frame []byte, childID string) bool {
	var hdr struct {
		Type    string `json:"type"`
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return false
	}
	return hdr.Type == protocol.TypeCtrlChildExited && hdr.ChildID == childID
}
