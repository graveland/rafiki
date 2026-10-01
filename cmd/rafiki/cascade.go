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

const includeSubagentsFlag = "include-subagents"

func addIncludeSubagentsFlag(cmd *cobra.Command, verb string) {
	cmd.Flags().Bool(includeSubagentsFlag, false, "Also "+verb+" every subagent beneath each target. Required (true or false) when a target has subagents")
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
