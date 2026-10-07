// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestSpawnerPrefillShaping pins that applySpawnSpecShaping copies the
// pre-fill list onto the spawn request verbatim, and that a nil spec list
// leaves req.Prefill nil (absent means no pre-fill).
func TestSpawnerPrefillShaping(t *testing.T) {
	t.Run("copied", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{
			Prefill: []protocol.PrefillRead{
				{Path: "CLAUDE.md"},
				{Path: "pkg/prefill/prefill.go", Start: 10, End: 40},
			},
		})
		assert.NewCollecting(t).False(len(req.Prefill) != 2 ||
			req.Prefill[0].Path != "CLAUDE.md" ||
			req.Prefill[1].Path != "pkg/prefill/prefill.go" ||
			req.Prefill[1].Start != 10 || req.Prefill[1].End != 40, "req.Prefill = %#v, want the two spec entries verbatim", req.Prefill)
	})

	t.Run("nil stays nil", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{})
		assert.NewCollecting(t).Nil(req.Prefill, "req.Prefill")
	})
}

// TestSpawnerSandboxShaping pins that applySpawnSpecShaping copies
// SpawnSpec.Sandbox onto protocol.SpawnRequest.Sandbox verbatim, and that a nil
// spec sandbox leaves req.Sandbox nil (absent means no sandbox block). Both the
// child-bound spawner (controllerSpawner.Spawn, agent_spawner.go) and the
// user-bound one (userSpawner.Spawn, user_spawner.go) build their request and
// then call this ONE helper, so deleting the `req.Sandbox = spec.Sandbox` line
// fails here rather than silently dropping every spawn block.
func TestSpawnerSandboxShaping(t *testing.T) {
	t.Run("copied", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{
			Sandbox: &protocol.SandboxSpec{
				Scope:   protocol.ScopeSubtree,
				Image:   "rafiki/sandbox:1",
				Network: protocol.NetworkNone,
				Mounts:  []protocol.SandboxMount{{Target: "/work", Kind: protocol.MountRO, HostPath: "/srv/repos/a"}},
			},
		})
		c := assert.NewAborting(t)
		c.Require().NotNil(req.Sandbox, "req.Sandbox is nil; the shaping copy is gone")
		c.Eq(protocol.ScopeSubtree, req.Sandbox.Scope, "Scope")
		c.Eq("rafiki/sandbox:1", req.Sandbox.Image, "Image")
		c.Eq(protocol.NetworkNone, req.Sandbox.Network, "Network")
		c.Len(req.Sandbox.Mounts, 1, "Mounts")
	})

	t.Run("nil stays nil", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{})
		assert.NewCollecting(t).Nil(req.Sandbox, "req.Sandbox")
	})
}
