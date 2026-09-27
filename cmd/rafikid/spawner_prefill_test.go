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
