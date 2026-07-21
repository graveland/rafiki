package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"git.graveland.dev/brent/fundi/client"
	"git.graveland.dev/brent/fundi/protocol"
)

func newLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label <id> [k=v ...]",
		Short: "Add, update, or remove labels on a child",
		Long: `Add, update, or remove labels on a running or exited child.

Labels are arbitrary key=value metadata. Specify k=v pairs as positional
arguments to set or update labels. Use --remove to delete existing keys.

The pic/ prefix is reserved for auto-labels set by the daemon (pic/model,
pic/cwd, etc.) and cannot be set or removed via this command.`,
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
	c := mustDial(cmd)
	defer c.Close()

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

	childID, err := c.Resolve(ctx, target)
	if err != nil {
		return err
	}

	req := protocol.SetLabelsRequest{
		Type:    protocol.TypeCtrlSetLabels,
		ChildID: childID,
		Set:     set,
		Remove:  removeKeys,
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_set_labels: %s", client.FormatError(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	var data protocol.SetLabelsResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return enc.Encode(json.RawMessage(resp.Data))
	}
	return enc.Encode(data)
}
