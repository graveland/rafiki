package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get [id|name...]",
		Short:   "Show details for one or more children",
		Aliases: []string{"show", "info"},
		Args:    cobra.MinimumNArgs(1),
		RunE:    runGet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)

	var children []protocol.ChildSummary
	var failures int
	for _, arg := range args {
		childID, err := c.Resolve(ctx, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		resp, err := c.Request(ctx, protocol.GetRequest{
			Type:    protocol.TypeCtrlGet,
			ChildID: childID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: get %q: %v\n", arg, err)
			failures++
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "error: get %q: %s\n", arg, client.FormatError(resp))
			failures++
			continue
		}
		var child protocol.ChildSummary
		if err := json.Unmarshal(resp.Data, &child); err != nil {
			fmt.Fprintf(os.Stderr, "error: decode %q: %v\n", arg, err)
			failures++
			continue
		}
		children = append(children, child)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	// Single successful target: emit plain object for backward compatibility.
	// Multiple targets (or any failures): wrap in {"children":[...]}.
	if len(args) == 1 && failures == 0 && len(children) == 1 {
		if err := enc.Encode(children[0]); err != nil {
			return err
		}
	} else {
		if err := enc.Encode(map[string]any{"children": children}); err != nil {
			return err
		}
	}

	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}
