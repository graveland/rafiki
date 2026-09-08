// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newBudgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "budget",
		Short: "Change a child's budget with operator authority",
	}
	cmd.AddCommand(newBudgetSetCmd())
	return cmd
}

func newBudgetSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <id|name> <amount>",
		Short: "Set a child's max-cost cap (0 or --unlimited clears it)",
		Long: "Sets a child's budget with OPERATOR authority: unlike the\n" +
			"agent-facing agent_set_budget tool, this may target any child at\n" +
			"any depth (not just a direct child of the caller) and is never\n" +
			"checked against a parent's remaining budget.\n\n" +
			"amount is in your configured display currency (see `rafiki config`).\n" +
			"Pass 0 or --unlimited to remove the cap entirely.",
		Args: cobra.RangeArgs(1, 2),
		RunE: runBudgetSet,
	}
	cmd.Flags().Bool("unlimited", false, "clear the cap (equivalent to passing 0)")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runBudgetSet(cmd *cobra.Command, args []string) error {
	unlimited, _ := cmd.Flags().GetBool("unlimited")
	if !unlimited && len(args) != 2 {
		return fmt.Errorf("amount is required unless --unlimited is set")
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

	var maxCost float64
	if !unlimited {
		raw := strings.TrimSpace(args[1])
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("amount %q is not a number", raw)
		}
		if v < 0 {
			return fmt.Errorf("amount cannot be negative; pass 0 or --unlimited to clear the cap")
		}
		maxCost = costfmt.ToUSD(v, clientstate.LoadScoped(clientstate.Scope{}).Currency)
	}

	resp, err := client.SetBudget(ctx, connect.NewRequest(&rafikiv1.SetBudgetRequest{
		ChildId: childID,
		MaxCost: maxCost,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	if resp.Msg.GetMaxCost() == 0 {
		fmt.Printf("%s: budget cleared (unlimited)\n", childID)
	} else {
		fmt.Printf("%s: budget set to %s\n", childID,
			costfmt.Format(resp.Msg.GetMaxCost(), clientstate.LoadScoped(clientstate.Scope{}).Currency))
	}
	return nil
}
