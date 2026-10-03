// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// newRouteCmd is the TOP-LEVEL `rafiki route` group: steering a running
// child's provider routing spec. It is deliberately not a subcommand of
// `providers` — `rafiki providers route` already names the per-model-line
// POLICY rows, a different feature that must not be touched.
func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Steer a running child's provider routing",
		Long: `Change where a running child's OpenRouter requests are served, without
restarting it. The daemon merges the spec you give over the child's stored
spec and the child obeys it on its very next request, so a coordinator can
act on what a child's traffic is doing right now.

Subcommands:
  set           merge a routing-spec delta over a child's stored spec`,
	}
	cmd.AddCommand(newRouteSetCmd())
	return cmd
}

func newRouteSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <child> <spec>",
		Short: "Steer a running child's provider routing (takes effect on its next request)",
		Long: `Merge a routing-spec delta over a running child's stored routing spec. The
delta is merged per key over what the child already carries, and takes effect
on the child's next OpenRouter request — no restart, no re-resolve of the
spawn or preset policy. The daemon returns the canonical spec after the merge
and that is what this verb prints.

CHILD is the child to steer; the daemon looks it up by id. SPEC is a
bracket-free routing spec — the same grammar a spawn, a preset or a model
string's "[...]" bracket uses, without the brackets — comma-separated and
order-free:

  sort=price|throughput|latency|balanced
  quant=<floor>+            e.g. fp8+ — that tier and every tier above it
  quant=a|b|c               an explicit list; a floor cannot join a list
  prefer=slug|slug          preferred providers, tried FIRST but fallen back
                            FROM: an unhealthy or banned one is skipped
  only=slug|slug            serve ONLY these providers — a HARD PIN, no
                            fallback: prefer= orders, only= restricts
  nodata, zdr               bare flags: no prompt-retaining hosts / ZDR only

prefer= is the soft form and only= the hard one: prefer= sets the order
OpenRouter tries providers in and still falls back to the rest, while only=
removes every provider not named, so an outage or a ban on the only= set has
nowhere to fall back to. A child credential may set prefer=, sort= and quant=
but may never set or change only= (that would sidestep the operator's bans),
nor clear an operator-set only=, nodata or zdr.

The merge never clears a key you do not name: a delta of "sort=price" leaves
the child's prefer=, only= and quant= exactly as they were. Steering a key an
operator owns from a child credential is refused with permission_denied.

Example:
  rafiki route set c_01HXABC 'prefer=fireworks|deepinfra'
      Try Fireworks first, then DeepInfra, and fall back to the rest.
  rafiki route set c_01HXABC 'sort=throughput,nodata'
      Fastest provider that will not retain the prompt.
  rafiki route set c_01HXABC 'only=fireworks'
      Pin the child to Fireworks alone (operator authority). Quote the
      spec: "|" is a shell pipe.

The child id and canonical spec print on success; -j/-J emit the daemon's
response as protojson instead. A refused steer — permission_denied,
invalid_argument for an unparseable spec, not_found for an unknown child —
prints on stderr and exits non-zero.`,
		Args: cobra.ExactArgs(2),
		RunE: runRouteSet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runRouteSet(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().SetRouting(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: args[0], Delta: args[1]}))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}

	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	if mode == outputJSON || mode == outputJSONL {
		return emitProto(cmd.OutOrStdout(), resp.Msg, mode)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s  routing: %s\n", resp.Msg.GetChildId(), resp.Msg.GetRouting())
	return nil
}
