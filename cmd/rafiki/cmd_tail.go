// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tail [id|name]",
		Aliases: []string{"stream"},
		Short:   "Follow a child's event stream, or the whole fleet's",
		Long: `Follow events as they happen: ` + "`rafiki tail [id]`" + ` is ` + "`rafiki logs -f -n 20`" + `.

With a child it backfills the child's last 20 history events and then follows
the live stream, exiting when the child exits. With no child it streams from
every child you can see (lifecycle events only), starting now and running
until you stop it — a child spawned later appears with no reopening, exactly
like the rail.

--label k=v (repeatable) and --has-label k (repeatable) narrow the fleet-wide
stream to children whose labels match; they are mutually exclusive with a
child argument. The terms join into one label selector — the same grammar the
control plane documents: ` + "`a=b,c`" + `, a bare ` + "`k`" + ` meaning "has label k".

The rendered view is one line per event. --types narrows to named event
types; --all-types widens to every type, including the ephemeral deltas; -r
emits one protojson Event per line instead of rendering.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runTail,
	}
	cmd.Flags().StringSlice("label", nil, "Match children with label k=v (repeatable; mutually exclusive with [id])")
	cmd.Flags().StringSlice("has-label", nil, "Match children that have label key k (repeatable)")
	cmd.Flags().IntP("tail", "n", 20, "Backfill the last N events before following (-1 = all, 0 = none)")
	cmd.Flags().StringSlice("types", nil, "Only these event types (comma-separated)")
	cmd.Flags().Bool("all-types", false, "Every event type, plus the ephemeral deltas (tier ALL)")
	cmd.Flags().BoolP("raw", "r", false, "One protojson Event per line instead of the rendered view")

	_ = cmd.RegisterFlagCompletionFunc("label", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelPairs(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("has-label", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelKeys(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("types", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return prefixCompletions(allNativeTypes(), toComplete), cobra.ShellCompDirectiveNoFileComp
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
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()
	ctx := cmdCtx(cmd)

	labelPairs, _ := cmd.Flags().GetStringSlice("label")
	hasLabelKeys, _ := cmd.Flags().GetStringSlice("has-label")
	selector, err := buildLabelSelector(labelPairs, hasLabelKeys)
	if err != nil {
		return err
	}
	// A child argument and a label selector are mutually exclusive: one names
	// a child, the other narrows the fleet-wide subject.
	if len(args) > 0 && selector != "" {
		return fmt.Errorf("specify either an id/name or --label/--has-label, not both")
	}

	q := eventQuery{
		follow:  true,
		tailN:   20,
		subject: allSubject(selector),
	}
	if len(args) > 0 {
		childID, err := resolveTargetConnect(ctx, client, mustProfile(cmd).Name, args[0], ep.describe)
		if err != nil {
			return err
		}
		// Best-effort: update the active marker so subsequent no-arg commands
		// default to this child.
		_ = setActive(mustProfile(cmd).Name, childID)
		q.childID = childID
		q.subject = childSubject(childID)
	}

	tailN, _ := cmd.Flags().GetInt("tail")
	raw, _ := cmd.Flags().GetBool("raw")
	allTypes, _ := cmd.Flags().GetBool("all-types")
	explicit, _ := cmd.Flags().GetStringSlice("types")
	q.types, q.tier, err = resolveTypeFilter(explicit, allTypes, q.childID != "")
	if err != nil {
		return err
	}
	q.tailN = tailN
	q.raw = raw
	q.mode = mode

	return runEventQuery(ctx, ep, client, q, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// buildLabelSelector joins --label k=v pairs and --has-label k keys into one
// label selector, comma-joined per control.proto's grammar, in the order
// given: a bare key means presence. The values reuse the framed-era label
// validation (parseLabelFilterPairs / parseLabelFilterKeys, which permit the
// rafiki/ auto-labels a fleet filter legitimately names).
//
// A term that could poison the join is refused up front: comma and whitespace
// are grammar on the wire (term separator, and ParseSelector rejects embedded
// spaces), and a value carrying either would silently read as two terms —
// narrowing where the user meant one label.
func buildLabelSelector(pairs, keys []string) (string, error) {
	if len(pairs) == 0 && len(keys) == 0 {
		return "", nil
	}
	// Validation only; the raw strings carry the order into the join.
	if _, err := parseLabelFilterPairs(pairs); err != nil {
		return "", fmt.Errorf("--label: %w", err)
	}
	if _, err := parseLabelFilterKeys(keys); err != nil {
		return "", fmt.Errorf("--has-label: %w", err)
	}
	terms := make([]string, 0, len(pairs)+len(keys))
	for _, term := range append(append([]string{}, pairs...), keys...) {
		if strings.ContainsAny(term, ", \t\n") {
			return "", fmt.Errorf("label term %q contains a selector separator (comma or whitespace); give it as its own --label/--has-label", term)
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, ","), nil
}
