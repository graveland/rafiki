package main

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

const (
	includeSubagentsFlag    = "include-subagents"
	subagentsOnlyFlag       = "subagents-only"
	exitedSubagentsOnlyFlag = "exited-subagents-only"
)

func addIncludeSubagentsFlag(cmd *cobra.Command, verb string) {
	cmd.Flags().Bool(includeSubagentsFlag, false, "Also "+verb+" every subagent beneath each target. Required (true or false) when a target has subagents")
	cmd.Flags().Bool(subagentsOnlyFlag, false, "Only "+verb+" the subagents beneath each target, leaving the target itself alone")
	cmd.MarkFlagsMutuallyExclusive(includeSubagentsFlag, subagentsOnlyFlag)
}

// addExitedSubagentsOnlyFlag is close's extra answer: finalize the subagents
// that have already exited and touch nothing else.
func addExitedSubagentsOnlyFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(exitedSubagentsOnlyFlag, false, "Only close the already-exited subagents beneath each target (never stops anything, keeps the target and any exited subagent with a live one beneath it)")
	cmd.MarkFlagsMutuallyExclusive(includeSubagentsFlag, exitedSubagentsOnlyFlag)
	cmd.MarkFlagsMutuallyExclusive(subagentsOnlyFlag, exitedSubagentsOnlyFlag)
}

// legacyInclude is the include_descendants value that agrees with scope. It is
// sent alongside scope so a daemon that predates DescendantScope still does the
// right thing for the two scopes it understands; it cannot honour the others.
func legacyInclude(scope rafikiv1.DescendantScope) bool {
	return scope == rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS
}

// flagSet reports whether name is a bool flag on cmd that is true. A flag the
// command does not define reads as false.
func flagSet(cmd *cobra.Command, name string) bool {
	if cmd.Flags().Lookup(name) == nil {
		return false
	}
	v, _ := cmd.Flags().GetBool(name)
	return v
}

// descendantScope decides which of a target's rows a stop or close touches.
// --subagents-only and --exited-subagents-only each name their scope outright;
// otherwise it is includeSubagents' decision between the target alone and the
// whole subtree.
func descendantScope(ctx context.Context, cmd *cobra.Command, ctrl rafikiv1connect.ControlClient, childID, arg string, closing bool) (rafikiv1.DescendantScope, error) {
	switch {
	case flagSet(cmd, subagentsOnlyFlag):
		return rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY, nil
	case flagSet(cmd, exitedSubagentsOnlyFlag):
		return rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY, nil
	}
	include, err := includeSubagents(ctx, cmd, ctrl, childID, arg, closing)
	if err != nil {
		return rafikiv1.DescendantScope_DESCENDANT_SCOPE_UNSPECIFIED, err
	}
	if include {
		return rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS, nil
	}
	return rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY, nil
}

// subagentsOf returns the descendants of id in children that a stop or close
// takes a position on: Task subagents the proxy synthesized are left out, as
// they end and close with their parent without being asked. onlyLive narrows it
// to those still running, which is what a stop cares about; a close cares about
// every one, since closing the parent of an exited child strands its row.
func subagentsOf(children []*rafikiv1.ChildSummary, id string, onlyLive bool) []*rafikiv1.ChildSummary {
	parent := effectiveParents(children)
	var out []*rafikiv1.ChildSummary
	for _, ch := range children {
		if ch.GetLabels()[rail.NativeSubagentLabel] == "1" {
			continue
		}
		if onlyLive && ch.GetStatus() == string(protocol.StatusExited) {
			continue
		}
		cur := ch.GetChildId()
		for range len(children) {
			p, ok := parent[cur]
			if !ok {
				break
			}
			if p == id {
				out = append(out, ch)
				break
			}
			cur = p
		}
	}
	return out
}

// includeSubagents decides whether a stop or close of childID takes its
// subagents with it. An explicit --include-subagents=true|false is obeyed
// either way; left unset, a target with subagents is refused rather than
// guessed at, since either default is a surprise: ending one agent while its
// children carry on unsupervised, or ending a tree the caller named one node
// of.
func includeSubagents(ctx context.Context, cmd *cobra.Command, ctrl rafikiv1connect.ControlClient, childID, arg string, closing bool) (bool, error) {
	if cmd.Flags().Changed(includeSubagentsFlag) {
		return cmd.Flags().GetBool(includeSubagentsFlag)
	}
	resp, err := ctrl.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return false, fmt.Errorf("list children: %s", formatConnectErr(err))
	}
	kids := subagentsOf(resp.Msg.GetChildren(), childID, !closing)
	if len(kids) == 0 {
		return false, nil
	}
	state := "running "
	if closing {
		state = ""
	}
	return false, fmt.Errorf("%s has %d %ssubagent(s); re-run with --%s=true to include them or --%s=false to leave them",
		arg, len(kids), state, includeSubagentsFlag, includeSubagentsFlag)
}
