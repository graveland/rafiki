// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

// agent_route is agent_set_budget's sibling: a thin binding over the daemon's
// one authority point. These tests run at the spawner level, where the
// authority rule lives (Controller.SetChildRouting), exactly as
// agent_spawner_test.go tests SetBudget.

// A child credential may set prefer/sort/quant but not only= — the hardest of
// the D4 rules, and the one the MCP face and the Connect RPC must agree on
// because both route through Controller.SetChildRouting.
func TestAgentRouteFromChildCannotSetOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "prefer=fireworks")
	sp := newControllerSpawner(c, "c_mid") // c_mid owns c_leaf

	_, err := sp.SetRouting(context.Background(), "c_leaf", "only=deepinfra")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "a child may not pin providers with only=")
	ck.Eq("prefer=fireworks", storedRouting(t, c, "c_leaf"), "stored spec unchanged after the refusal")

	// The same caller may set what it IS allowed to set, so the refusal above
	// is only= and not a blanket refusal of the verb.
	got, err := sp.SetRouting(context.Background(), "c_leaf", "sort=throughput")
	ck.NoError(err, "a child may set sort=")
	ck.Eq("sort=throughput,prefer=fireworks", got, "merged spec")
}

// Steering is subtree authority: a target outside the spawner's own lineage is
// refused by Controller.SetChildRouting before anything is written.
func TestAgentRouteRefusesTargetOutsideSubtree(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_other", "nodata")
	sp := newControllerSpawner(c, "c_mid")

	_, err := sp.SetRouting(context.Background(), "c_other", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "a target outside the caller's subtree")
	ck.Eq("nodata", storedRouting(t, c, "c_other"), "stored spec unchanged after the refusal")
}

// The tool must reach CHILD authority and never operator authority. The
// controllerSpawner is a concrete Controller, so "which entry point ran" is
// read off the refusal each path produces for a target the two disagree on:
// SetChildRouting runs the lineage guard first (PermissionDenied, and the
// message names the subtree), while SetChildRoutingAsOperator skips it and
// reaches steer's Get (NotFound for an unknown child, success for self). A
// spawner that took the operator path would answer NotFound — or steer the
// caller's own spec — and this test would fail.
func TestAgentRouteNeverUsesOperatorAuthority(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	sp := newControllerSpawner(c, "c_mid")

	// Unknown child: the lineage guard refuses it. Operator authority would
	// have skipped the guard and reported NotFound instead.
	_, err := sp.SetRouting(context.Background(), "c_ghost", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "an unknown target is refused by the lineage guard")
	ck.StrContains(err.Error(), "subtree", "the refusal is the child-authority one; got %v", err)

	// Self: a caller is not a descendant of itself. Operator authority would
	// have steered c_mid's own spec.
	_, err = sp.SetRouting(context.Background(), "c_mid", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "a caller may not steer itself")
	ck.Eq("", storedRouting(t, c, "c_mid"), "the caller's own spec was not written")

	// And a genuine descendant does steer, so the refusals above are the
	// lineage guard and not a verb that refuses everything.
	got, err := sp.SetRouting(context.Background(), "c_leaf", "prefer=fireworks")
	ck.NoError(err, "a descendant is steerable")
	ck.Eq("prefer=fireworks", got, "canonical spec")
}

// The tool and the MCP face's blueprint list change together: a tool
// registered nowhere is never offered to a model.
func TestAgentRouteIsInMCPFaceToolList(t *testing.T) {
	ck := assert.NewAborting(t)
	var found bool
	for _, bp := range mcpBlueprints {
		if bp.Name() == "agent_route" {
			found = true
			break
		}
	}
	ck.True(found, "mcpBlueprints must contain agent_route")
}

// agent_route is a spawner-bound verb, like every other agent_* tool: without
// an AgentSpawner it must decline rather than register a tool that can only
// fail.
func TestAgentRouteDeclinesWithoutASpawner(t *testing.T) {
	ck := assert.NewAborting(t)
	tool, err := tools.AgentRouteBlueprint{}.Materialize(tools.ToolOpts{})
	ck.NoError(err, "Materialize")
	ck.Nil(tool, "no spawner must decline the tool")
}
