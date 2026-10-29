// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newModelsAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Your model aliases: short names for full model strings",
		Long: `Your model aliases: short names for full model strings.

A model string without a "/" anywhere the control surface accepts one is one of
your aliases (or an error); anything with a slash is a model id. So an alias
works wherever a model is accepted: rafiki create --model, a preset's model,
and agent_spawn.

An alias's target is provider/id, optionally carrying a routing bracket in the
model-string grammar, e.g.
'openrouter/~deepseek/deepseek-flash-latest[sort=price]'. Brackets on the alias
itself ('ds[sort=latency]') merge over the target's, key by key, and the
caller's keys win.

Aliases are strictly per user, and resolution happens per use: setting,
changing or deleting one affects future spawns only — never a running or
resumed child.

Subcommands:
  set <name> <target>   define or replace an alias
  list (ls)             the aliases you own, and their targets
  delete <name>         remove an alias`,
	}
	cmd.AddCommand(newModelsAliasSetCmd(), newModelsAliasListCmd(), newModelsAliasDeleteCmd())
	return cmd
}

func newModelsAliasSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <name> <target>",
		Short: "Define or replace one of your model aliases",
		Long: `Define or replace one of your model aliases: a slash-free short name, and the
full model string it stands for.

NAME must match ^[a-z0-9][a-z0-9._-]*$ — a name without a "/" or a "[" can
never be mistaken for a model id. TARGET is provider/id, optionally carrying a
routing bracket. The daemon validates both and refuses what it cannot parse; it
does NOT check the target against the catalog or your configured providers, so
an alias for a model you have not configured yet still stores.

Example:
  rafiki models alias set ds 'openrouter/~deepseek/deepseek-flash-latest[sort=price]'
      Quote the argument: the shell would expand "~" and treat "[" as a glob.
  rafiki models alias set fast 'openrouter/z-ai/glm-5.3[sort=latency]'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().PutModelAlias(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.PutModelAliasRequest{Name: args[0], Target: args[1]}))
			if err != nil {
				return err
			}
			// The daemon echoes what it stored; the arguments are the fallback
			// for a daemon that did not.
			name, target := args[0], args[1]
			if row := resp.Msg.GetRow(); row != nil {
				if row.GetName() != "" {
					name = row.GetName()
				}
				if row.GetTarget() != "" {
					target = row.GetTarget()
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s → %s\n", name, target)
			return nil
		},
	}
}

func newModelsAliasListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your model aliases and the model strings they stand for",
		Long: `List the model aliases YOU own, newest write first, with when each was last
changed. Nobody else's aliases are ever listed: they are per user, with no
shared layer beneath them.

Example:
  rafiki models alias list
  rafiki models alias list -J`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListModelAliases(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitModelAliases(cmd.OutOrStdout(), resp.Msg, mode, useColor)
		},
	}
}

func newModelsAliasDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove one of your model aliases",
		Long: `Remove one of your model aliases. A spawn that already resolved it is
unaffected; the next spawn naming it is refused with invalid_argument, the way
any unknown slash-free model string is. An alias that does not exist is refused
with not_found.

Example:
  rafiki models alias delete ds`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeModelAliasArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			if _, err := ep.control().DeleteModelAlias(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeleteModelAliasRequest{Name: args[0]})); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted alias %s\n", args[0])
			return nil
		},
	}
}

// completeModelAliasArgs offers the caller's alias names for `models alias
// delete`, as cobra wants them.
func completeModelAliasArgs(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeModelAliasNames(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeModelAliasNames returns the caller's alias names that start with
// toComplete. It resolves the endpoint through newConnectEndpoint — the one
// place the CLI resolves one — and swallows every error: a completion handler
// must never exit, never block past completionDeadline, and never print. The
// list is small and changes on the user's own write, so nothing is cached.
func completeModelAliasNames(cmd *cobra.Command, toComplete string) []string {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil // remote with no token: a 401 is the only possible outcome
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	resp, err := ep.control().ListModelAliases(ctx,
		connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(resp.Msg.GetRows()))
	for _, r := range resp.Msg.GetRows() {
		if name := r.GetName(); name != "" {
			names = append(names, name)
		}
	}
	return filterByPrefix(names, toComplete)
}

// emitModelAliases writes ListModelAliases' output in the resolved mode: -j is
// the response's canonical protojson (the rows envelope), -J one compact row
// per line, and the table is NAME, TARGET, UPDATED.
func emitModelAliases(w io.Writer, resp *rafikiv1.ListModelAliasesResponse, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return emitProto(w, resp, mode)
	case outputJSONL:
		return emitProtoRows(w, resp.GetRows(), mode)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "NAME", "TARGET", "UPDATED")...)
		for _, row := range resp.GetRows() {
			tb.Row(row.GetName(), defaultDash(row.GetTarget()), routeSince(row.GetUpdatedAt()))
		}
		return tb.Render()
	}
}
