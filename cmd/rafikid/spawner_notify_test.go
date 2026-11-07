// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestSpawnerForwardsNotify pins that SpawnSpec.Notify lands on the spawn
// request through applySpawnSpecShaping, the one helper controllerSpawner.Spawn
// and userSpawner.Spawn both call, so the two cannot drift.
func TestSpawnerForwardsNotify(t *testing.T) {
	for _, v := range []string{"", "all", "result"} {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{Notify: v})
		assert.NewCollecting(t).Eq(v, req.Notify, "req.Notify for %q", v)
	}
}

func TestMCPFaceSpawnDescriptionQualifiesNotification(t *testing.T) {
	c := assert.NewCollecting(t)
	spawnDesc := mcpToolDescriptions["agent_spawn"]
	c.StrContains(spawnDesc, `If you spawned it with notify="result", you are notified only when it records a result, asks a question, errors, exceeds its own budget or exits.`,
		"agent_spawn: the notify qualification is gone")
}
