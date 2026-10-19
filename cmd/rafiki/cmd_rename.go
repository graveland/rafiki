// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <id|name> <new-name>",
		Short: "Rename a live child and its conversations",
		Long: "Renames a live child, and every conversation linked to it, so the\n" +
			"cockpit, `rafiki ls` and conversation search/recall all show the\n" +
			"same name. A closed child is no longer registered and cannot be\n" +
			"renamed.",
		Args: cobra.ExactArgs(2),
		RunE: runRename,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func runRename(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[1])
	if name == "" {
		return fmt.Errorf("new name cannot be empty")
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()
	ctx := cmdCtx(cmd)

	childID, err := resolveChildConnect(ctx, ep, client, args[0])
	if err != nil {
		return err
	}
	resp, err := client.Rename(ctx, connect.NewRequest(&rafikiv1.RenameRequest{
		ChildId: childID,
		Name:    name,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	fmt.Printf("%s: renamed to %q\n", childID, resp.Msg.GetName())
	return nil
}
