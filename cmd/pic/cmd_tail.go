package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"git.graveland.dev/brent/pi-controller/client"
	"git.graveland.dev/brent/pi-controller/protocol"
)

func newTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail [id|name]",
		Short: "Stream events from a child",
		Long: `Subscribe to a child's event stream and render events as they arrive.

By default, token-by-token message_update deltas are suppressed (--no-deltas=true).
Use --profile to select a named filter preset, or --include/--exclude to customize.

In per-child mode, pic tail exits automatically when the child exits.
In label-filtered mode (--label/--has-label), pic tail streams from all matching
children and exits only on SIGINT/SIGTERM.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runTail,
	}
	cmd.Flags().String("profile", "", "Subscription profile: firehose|results|coarse|lifecycle")
	cmd.Flags().StringSlice("include", nil, "Add event types to subscription (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude event types from subscription (repeatable)")
	cmd.Flags().Bool("no-deltas", true, "Suppress token-by-token message_update deltas (default true)")
	cmd.Flags().BoolP("verbose", "v", false, "Include internal RPC response frames (autocomplete fetches, get_state, etc.)")
	cmd.Flags().StringSlice("label", nil, "Subscribe to all children matching label k=v (repeatable; mutually exclusive with [id])")
	cmd.Flags().StringSlice("has-label", nil, "Subscribe to all children that have label key k (repeatable)")
	cmd.Flags().IntP("tail", "n", 20, "Backfill the last N events before following (-1 = all in buffer, 0 = none)")
	cmd.Flags().Bool("raw", false, "Emit raw event frames (JSONL) instead of the rendered view")

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
	_ = cmd.RegisterFlagCompletionFunc("label", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelPairs(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("has-label", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelKeys(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
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

	labelPairs, _ := cmd.Flags().GetStringSlice("label")
	hasLabelKeys, _ := cmd.Flags().GetStringSlice("has-label")

	labels, err := parseLabelFilterPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("--label: %w", err)
	}
	hasLabels, err := parseLabelFilterKeys(hasLabelKeys)
	if err != nil {
		return fmt.Errorf("--has-label: %w", err)
	}

	labelFiltered := len(labels) > 0 || len(hasLabels) > 0

	// Validate: id/name and label filter are mutually exclusive.
	if len(args) > 0 && labelFiltered {
		return fmt.Errorf("specify either an id/name or --label/--has-label, not both")
	}

	profile, _ := cmd.Flags().GetString("profile")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")
	noDeltas, _ := cmd.Flags().GetBool("no-deltas")
	if noDeltas {
		exclude = append(exclude, "message_update")
	}

	mode, useColor := outputOpts(cmd)
	verbose, _ := cmd.Flags().GetBool("verbose")
	tailN, _ := cmd.Flags().GetInt("tail")
	raw, _ := cmd.Flags().GetBool("raw")

	if labelFiltered {
		events, cancelSub, err := c.Subscribe()
		if err != nil {
			return err
		}
		defer cancelSub()
		return runTailLabeled(ctx, c, events, labels, hasLabels, profile, include, exclude, mode, useColor, verbose)
	}
	return runTailChild(ctx, c, args, tailN, raw, profile, include, exclude, mode, useColor, verbose)
}

// runTailLabeled handles the label-filtered subscription mode.  It subscribes
// to events from all children matching the label filter and streams them with
// a per-child ID prefix so multi-child output is readable.
func runTailLabeled(
	ctx context.Context,
	c *client.Client,
	events <-chan []byte,
	labels map[string]string,
	hasLabels []string,
	profile string, include, exclude []string,
	mode outputMode, useColor, verbose bool,
) error {
	subReq := protocol.SubscribeRequest{
		Type:     protocol.TypeCtrlSubscribe,
		Labels:   labels,
		HasLabel: hasLabels,
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

	// In label-filtered mode each event is prefixed with the short child ID so
	// multi-child output is parseable.
	pw := &linePrefixWriter{w: os.Stdout}
	renderer := newTailRenderer(pw, useColor, mode, verbose)

	for {
		select {
		case frame, ok := <-events:
			if !ok {
				return nil
			}
			// Set the per-event prefix before rendering.
			if cid := frameChildID(frame); cid != "" {
				pw.Prefix = "[" + shortChildID(cid) + "] "
			} else {
				pw.Prefix = ""
			}
			if err := renderer.render(frame); err != nil {
				if errors.Is(err, errDaemonShutdown) {
					return nil
				}
				fmt.Fprintln(os.Stderr, "render error:", err)
			}
			// In label-filtered mode we never auto-exit: other matching
			// children may still be active after any one child exits.
		case <-ctx.Done():
			return nil
		}
	}
}

// runTailChild handles the per-child mode: it backfills the last N events and
// then follows live via the shared history engine.
func runTailChild(
	ctx context.Context,
	c *client.Client,
	args []string,
	tailN int, raw bool,
	profile string, include, exclude []string,
	mode outputMode, useColor, verbose bool,
) error {
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
	return runHistoryOut(ctx, c, childID, historyOpts{
		follow:   true,
		tailN:    tailN,
		raw:      raw,
		profile:  profile,
		include:  include,
		exclude:  exclude,
		verbose:  verbose,
		mode:     mode,
		useColor: useColor,
	})
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

// frameChildID extracts the childId field from a frame, returning "" if absent
// or the frame is unparseable.
func frameChildID(frame []byte) string {
	var hdr struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return ""
	}
	return hdr.ChildID
}

// shortChildID returns up to 8 chars of id as a compact display token.
func shortChildID(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}
	return id[:n]
}
