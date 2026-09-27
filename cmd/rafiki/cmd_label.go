package main

import (
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "label <id> [k=v ...]",
		Aliases: []string{"lab"},
		Short:   "Add, update, or remove labels on a child",
		Long: `Add, update, or remove labels on a running or exited child.

Labels are arbitrary key=value metadata. Specify k=v pairs as positional
arguments to set or update labels. Use --remove to delete existing keys.

The rafiki/ prefix is reserved for auto-labels set by the daemon (rafiki/model,
rafiki/cwd, etc.) and cannot be set or removed via this command.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runLabel,
	}
	cmd.Flags().StringArray("remove", nil, "Remove a label key (repeatable)")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runLabel(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

	ctx := cmdCtx(cmd)

	target := args[0]
	kvPairs := args[1:]
	removeKeys, _ := cmd.Flags().GetStringArray("remove")

	if len(kvPairs) == 0 && len(removeKeys) == 0 {
		return fmt.Errorf("at least one k=v argument or --remove flag is required")
	}

	set, err := parseLabelPairs(kvPairs)
	if err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}
	for _, k := range removeKeys {
		if err := validateCLILabelKey(k); err != nil {
			return fmt.Errorf("--remove: %w", err)
		}
	}

	childID, err := resolveTargetConnect(ctx, ctrl, mustProfile(cmd).Name, target, ep.describe)
	if err != nil {
		return err
	}

	resp, err := ctrl.SetLabels(ctx, connect.NewRequest(&rafikiv1.SetLabelsRequest{
		ChildId: childID,
		Set:     set,
		Remove:  removeKeys,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	// Labels feed label completion; what the last TAB showed is now stale.
	dropChildCompletionCache(cmd)

	// Label has always answered with the post-mutation label map as JSON;
	// now it is the SetLabels response's canonical protojson.
	return emitProto(os.Stdout, resp.Msg, outputJSON)
}
