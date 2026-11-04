// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/compactfork"
	"go.graveland.dev/rafiki/pkg/costfmt"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// newModelsSwitchCmd is `rafiki models switch <child> <model>`: change the
// model a conversation runs on, which the daemon cannot do in place — a
// child's model resolves once at spawn — so this composes the fork path:
//
//  1. spawn a kind=fundi child FORKED from the source, on the new model,
//     carrying the source's cwd, user labels and spend cap
//  2. close the source (stopping it first when live)
//  3. rename the new child into the source's name
//  4. replay the source's routing spec, so operator pins survive
//
// From the rail the conversation looks like it changed models; in the
// database it is a new conversation on the old one's fork edge, and the old
// one stays queryable at its old model. Steps 2-4 each name themselves on
// failure: both conversations exist, and finishing by hand is safe.
func newModelsSwitchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "switch <child> <model>",
		Short: "Move a conversation onto a new model via a fork (clone → close → rename)",
		Long: `Switch the model a conversation runs on.

A child's model resolves once at spawn and cannot change in place, so a
switch is a FORK under the hood: a new kind=fundi child starts from the
source's last completed turn — borrowing its whole history, so nothing is
lost — then the source is closed and the new child takes its name. The new
conversation therefore continues seamlessly from the rail's point of view,
on any model the daemon offers, including cross-provider (a claude child's
conversation switches to an OpenRouter model this way).

What carries over: the history up to the fork point, the source's cwd, its
parent (a subagent's switch stays under the same coordinator), its user
labels, and its spend cap (--max-cost overrides). The cap is PER
CONVERSATION: the new child starts with the source's full cap; the source's
spend is not deducted from it. What does not: the claude kind itself (a fork
runs as fundi), anything the source's preset injected at ITS spawn, and live
subagents (left running; --descendants closes them too).

A source that is MID-TURN when the switch lands loses that in-flight turn
(and any tool side effects it caused); the fork starts from the last
completed turn. The routing spec is replayed as-is: it was written for the
old model's provider lines, and may misroute or be refused on the new one —
the step names itself if the replay fails.

A switch onto a model whose context window cannot hold the borrowed history is
REFUSED with both numbers rather than spawned to fail on its first call. Pass
--compact to fit it: the source is forked onto its own model as a temporary
helper child, that child is compacted, and the continuation forks from the
compacted result — so the new conversation starts from a summary of the old
one instead of its whole transcript. The helper is closed, never deleted, and
the source is still closed exactly once, by this command.

If a step after the spawn fails, the command says which: both conversations
exist, and the printed rafiki close/rename/route line finishes by hand.
Re-running the whole switch would spawn a SECOND fork.

  rafiki models switch web-researcher openrouter/z-ai/glm-5.3
  rafiki models switch c_01HXABC anthropic/claude-opus-5-5 --max-cost 5`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeSwitchArgs,
		RunE:              runModelsSwitch,
	}
	maxCostHelp := "budget for the switched conversation's whole subtree (default: the source's cap)"
	if cur := clientstate.LoadScoped(clientstate.Scope{}).Currency; cur != nil && cur.Rate > 0 && cur.Code != "" {
		maxCostHelp = fmt.Sprintf(
			"budget for the switched conversation's whole subtree, in %s (default: the source's cap); see `rafiki config`",
			cur.Code)
	}
	cmd.Flags().Float64("max-cost", -1, maxCostHelp)
	cmd.Flags().Bool("compact", false,
		"when the borrowed history exceeds <model>'s context window, compact the source on its own model\n"+
			"first (spawns a temporary helper child); without it an over-window switch is refused. A window\n"+
			"the daemon cannot resolve — including a source with no completed turn — never refuses")
	cmd.Flags().Bool("descendants", false, "Also close the source's subagents (default: leave them running)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout when the source must be stopped first (default 2.5s)")
	return cmd
}

// completeSwitchArgs offers children for the first word and, for the second,
// the models the daemon can run on a fundi child — a switch always lands on
// fundi, whatever the source was.
func completeSwitchArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	case 1:
		// A switch always lands on kind=fundi, whatever the source was, so
		// the candidates are fundi's model universe.
		return filterByPrefix(modelIDs(cmd, protocol.KindFundi), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func runModelsSwitch(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()
	ctx := cmdCtx(cmd)

	childID, err := resolveTargetConnect(ctx, ctrl, mustProfile(cmd).Name, args[0], ep.describe)
	if err != nil {
		return err
	}
	model := args[1]

	get, err := ctrl.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	src := get.Msg.GetChild()
	if src.GetKind() == protocol.KindScript {
		return fmt.Errorf("%s is a script child: it has no conversation history to fork", childID)
	}
	// A sandboxed source must not switch: the composition cannot carry the
	// sandbox block (ChildSummary does not expose it), so the switch would
	// land the continuation UNCONFINED on the default executor — a
	// confinement widening nobody asked for. Fork by hand with an explicit
	// sandbox block instead.
	if src.GetLabels()["rafiki/sandbox"] == "1" {
		return fmt.Errorf("%s runs in a sandbox and a switch cannot carry the sandbox; fork it by hand with an explicit --sandbox block instead", childID)
	}

	// Over-window detection, before anything is spawned: the continuation
	// borrows the source's whole history, and a model whose window cannot hold
	// it would take the fork and then fail on its first call. known is false
	// when the source has no completed turn or the daemon cannot price the
	// destination, and unknown never refuses.
	compactFlag, _ := cmd.Flags().GetBool("compact")
	tokens, window, known, err := forkOverflow(ctx, ep, ctrl, childID, model)
	if err != nil {
		return err
	}
	compacting := known && !modelquery.FitsWindow(tokens, window)
	if compacting && !compactFlag {
		return fmt.Errorf("borrowed history is ~%d tokens but %s's window is %d; pass --compact to compact it on %s first, or pick a larger model",
			tokens, model, window, src.GetModel())
	}
	if compacting {
		fmt.Fprintf(os.Stderr, "compacting ~%d tokens on %s (one summary call)\n", tokens, src.GetModel())
	} else if compactFlag {
		if known {
			fmt.Fprintf(os.Stderr, "borrowed history is ~%d tokens, within %s's %d-token window; not compacting\n", tokens, model, window)
		} else {
			fmt.Fprintln(os.Stderr, "borrowed history size or the destination's window is unknown; not compacting")
		}
	}

	// The source's parent, so a SUBAGENT's switch stays under the same
	// coordinator: dropping it would move the switch out of the coordinator's
	// lineage (budget, tag scopes, reports) and rename it against the wrong
	// sibling set. A top-level source has no parent label and stays top-level.
	// ParentLabel honours the pre-rename fundi/parent spelling, the same
	// fallback Rename and the lineage reads use.
	parent, _ := childstore.ParentLabel(src.GetLabels())

	req := &rafikiv1.SpawnRequest{
		ParentChildId: parent,
		// The source's cwd: a switch continues the same work, and Spawn
		// refuses a cwd its machine cannot see.
		Cwd:  src.GetCwd(),
		Kind: protocol.KindFundi,
		// Model may be slash-free: the daemon resolves an owner alias.
		Model: model,
		ForkFrom: &rafikiv1.SpawnRequest_ForkFrom{
			// Empty cursor: the source's last completed turn — the newest
			// point a fork may legally start after.
			ChildId: childID,
		},
	}
	labels := map[string]string{}
	for k, v := range src.GetLabels() {
		// Only USER labels carry over: rafiki/ and fundi/ are daemon
		// namespaces, and "owner" is reserved outright (it feeds executor
		// admission selectors) — spawning with any of them is refused.
		if strings.HasPrefix(k, "rafiki/") || strings.HasPrefix(k, "fundi/") || k == "owner" {
			continue
		}
		labels[k] = v
	}
	req.Labels = labels
	if cmd.Flags().Changed("max-cost") {
		v, _ := cmd.Flags().GetFloat64("max-cost")
		if v < 0 {
			return fmt.Errorf("--max-cost must be a non-negative amount (got %v); leave it off to carry the source's cap", v)
		}
		// On the wire an explicit 0 means "spend nothing" (SpawnRequest.max_cost's
		// own comment), which on a switch reads as an instant halt. If you meant
		// to LIFT the cap, that is not expressible here: blank carries the
		// source's cap, and the new child's cap is editable afterwards.
		if v == 0 {
			return fmt.Errorf("--max-cost 0 means \"spend nothing\" — a switched conversation that can never spend; leave it off to carry the source's cap")
		}
		usd := costfmt.ToUSD(v, clientstate.LoadScoped(clientstate.Scope{}).Currency)
		req.MaxCost = &usd
	} else if src.MaxCost != nil {
		req.MaxCost = src.MaxCost
	}

	// The source's preset re-resolves on the switch spawn: its tool, skill and
	// MCP allowlists and its prompt are part of "the same conversation", and
	// the preset's own model is overridden by the switch's explicit model
	// (preset resolves first, explicit fields win). A preset DELETED since the
	// source spawned refuses the switch — a safe failure, both conversations
	// remain. Plain --fork-from does NOT carry the preset: pass -p yourself.
	if pname := src.GetLabels()["rafiki/preset"]; pname != "" {
		req.Preset = pname
	}

	var newID string
	if compacting {
		newID, err = compactfork.Run(ctx, ctrl, req, compactfork.Options{
			SourceModel: src.GetModel(),
			DestWindow:  window,
			Progress:    func(s string) { fmt.Fprintln(os.Stderr, s) },
		})
		if err != nil {
			return err
		}
	} else {
		spawned, err := ctrl.Spawn(ctx, connect.NewRequest(req))
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		newID = spawned.Msg.GetChildId()
	}
	fmt.Printf("spawned %s on %s (fork of %s)\n", newID, model, childID)

	// Everything past here leaves BOTH conversations in place on failure;
	// each error names the step and the ids to finish by hand.
	scope := rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY
	if descendants, _ := cmd.Flags().GetBool("descendants"); descendants {
		scope = rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS
	}
	kt, _ := cmd.Flags().GetDuration("kill-timeout")
	if err := closeChildConnect(ctx, ctrl, childID, scope, 0, kt); err != nil {
		return fmt.Errorf("switch incomplete — %s spawned on %s, but closing %s failed: %w (finish with `rafiki close %s` + `rafiki rename %s <name>`)",
			newID, model, childID, err, childID, newID)
	}
	fmt.Printf("closed %s\n", childID)

	name := src.GetName()
	if name != "" {
		if _, err := ctrl.Rename(ctx, connect.NewRequest(&rafikiv1.RenameRequest{ChildId: newID, Name: name})); err != nil {
			return fmt.Errorf("switch incomplete — %s closed, but renaming %s to %q failed: %w (`rafiki rename %s %s` finishes it)",
				childID, newID, name, err, newID, name)
		}
		fmt.Printf("renamed %s to %s\n", newID, name)
	}
	if routing := src.GetRouting(); routing != "" {
		if _, err := ctrl.SetRouting(ctx, connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: newID, Delta: routing})); err != nil {
			return fmt.Errorf("switch incomplete — %s is renamed and closed, but replaying routing %q onto %s failed: %w (`rafiki route set %s <spec>` finishes it)",
				childID, routing, newID, err, newID)
		}
		fmt.Printf("routing %s replayed onto %s\n", routing, newID)
	}
	return nil
}
