// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestSpawnerScriptShaping pins the wave-5 plumbing: the pymodule_start
// tool's kind=script spec and labels reach the spawn request verbatim through
// the ONE shaping helper both spawners call — so a fundi child's spawn (the
// child-bound spawner) and an MCP user's (the user-bound one) cannot drift.
func TestSpawnerScriptShaping(t *testing.T) {
	t.Run("script spec and labels copied", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{
			Script: &protocol.ScriptSpec{Repo: "local", Script: "driver", Modules: []string{"helpers"}, Args: []string{"--fast"}},
			Labels: map[string]string{"env": "work"},
		})
		if req.Script == nil || req.Script.Repo != "local" || req.Script.Script != "driver" ||
			len(req.Script.Modules) != 1 || len(req.Script.Args) != 1 {
			t.Errorf("req.Script = %#v, want the spec verbatim", req.Script)
		}
		if req.Labels["env"] != "work" || len(req.Labels) != 1 {
			t.Errorf("req.Labels = %v, want only env=work", req.Labels)
		}
	})
	t.Run("nil stays nil for every other kind", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{})
		if req.Script != nil || req.Labels != nil {
			t.Errorf("req.Script/req.Labels = %#v/%#v, want nil (a fundi or claude spawn must not carry a script arm)", req.Script, req.Labels)
		}
	})
}
