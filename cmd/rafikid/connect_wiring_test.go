// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestControllerSatisfiesConnectSeams is a compile-time assertion with a
// runtime home: if a seam's signature drifts, this file stops compiling, which
// is the whole point. It is cheap and it is the only thing standing between a
// renamed method and a daemon that silently never wires its control plane.
func TestControllerSatisfiesConnectSeams(t *testing.T) {
	var _ connectapi.ChildLister = (*Controller)(nil)
	var _ connectapi.ChildLifecycle = connectLifecycle{}
	// DescendantLister is NOT part of ChildLifecycle: the cascade asserts it
	// off the lifecycle at runtime, so the compiler stays silent if the
	// adapter loses the method and every include_descendants request answers
	// "unimplemented". This line is the pin.
	var _ connectapi.DescendantLister = connectLifecycle{}
	var _ connectapi.ConversationResolver = (*Controller)(nil)
}

// buildProtocolSpawnRequest must carry every Connect-plane param onto the
// framed request Controller.Spawn applies — preset included, since the daemon
// resolves it first. An unmapped field is a silent drop on the cockpit's
// spawn path.
func TestBuildProtocolSpawnRequestCarriesFieldsThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	depth, children := 2, 3
	cost := 1.5
	got := buildProtocolSpawnRequest(connectapi.SpawnParams{
		Cwd: "/work", Name: "scout", Model: "claude-opus-5", Kind: "fundi",
		Preset: "reviewer", ParentChildID: "c_0",
		ExecutorSelector: "kind=native", ExecutorRef: "greyshift",
		Labels:      map[string]string{"team": "a"},
		MaxDepth:    &depth,
		MaxCost:     &cost,
		MaxChildren: &children,
	})
	c.Eq("reviewer", got.Preset, "Preset")
	c.False(got.Kind != "fundi" || got.Model != "claude-opus-5" || got.Name != "scout", "identity fields wrong: %+v", got)
	c.False(got.ParentChildID != "c_0" || got.ExecutorRef != "greyshift", "lineage/executor wrong: %+v", got)
	c.Eq("a", got.Labels["team"], "labels wrong: %+v", got.Labels)
	c.False(got.MaxDepth == nil || *got.MaxDepth != 2 || got.MaxCost == nil || *got.MaxCost != 1.5 || got.MaxChildren == nil || *got.MaxChildren != 3, "budgets wrong: %+v", got)
}

// TestConnectLifecycleForwardsDescendantIDs runs the seam above at runtime:
// the compile-time line proves the adapter HAS DescendantIDs, this proves it
// forwards to the Controller's list rather than answering empty. A parent and
// one non-native subagent are enough to see the forwarding.
func TestConnectLifecycleForwardsDescendantIDs(t *testing.T) {
	c := assert.NewAborting(t)
	st := childstore.New()
	now := time.Now()
	st.Insert(&childstore.Session{
		ChildID: "a", Status: protocol.StatusIdle, StartedAt: now, Labels: map[string]string{},
	})
	st.Insert(&childstore.Session{
		ChildID: "b", Status: protocol.StatusIdle, StartedAt: now,
		Labels: map[string]string{childstore.LabelParent: "a", childstore.LabelRoot: "a"},
	})

	lc := connectLifecycle{c: &Controller{st: st}}
	c.EqDeep([]string{"b"}, lc.DescendantIDs("a"), "adapter forwards to the Controller's descendants")
}
