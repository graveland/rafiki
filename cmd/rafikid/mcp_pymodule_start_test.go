// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// recordingSpawner is the smallest AgentSpawner a pymodule_start materialize
// test needs: it records the spec and answers with a fixed row. Authority
// rules live on the daemon-side spawners and are tested there; this fake
// exists so the FACE's gate and handoff can be pinned without a Controller.
type recordingSpawner struct {
	specs []tools.SpawnSpec
}

func (s *recordingSpawner) List(context.Context) ([]tools.AgentInfo, error) {
	return nil, nil
}
func (s *recordingSpawner) Models(context.Context, tools.ModelQuery) ([]tools.ModelInfo, error) {
	return nil, nil
}
func (s *recordingSpawner) Spawn(_ context.Context, spec tools.SpawnSpec) (tools.AgentInfo, error) {
	s.specs = append(s.specs, spec)
	return tools.AgentInfo{ChildID: "c_new", Name: spec.Name, Kind: spec.Kind, Status: "idle"}, nil
}
func (s *recordingSpawner) View(context.Context, string, int) (string, error) { return "", nil }
func (s *recordingSpawner) Send(context.Context, string, string) error        { return nil }
func (s *recordingSpawner) Kill(context.Context, string) error                { return nil }
func (s *recordingSpawner) SetBudget(context.Context, string, float64) error  { return nil }

// The gate mirrors pymodule_run's: nil PyModuleStarter declines, and the
// materialized tool is bound to PyModuleStarter — never to Agents — so the
// gate and the binding cannot drift apart.
func TestMCPPyModuleStartBlueprintGate(t *testing.T) {
	t.Run("declines without PyModuleStarter", func(t *testing.T) {
		c := assert.NewAborting(t)
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{})
		c.NoError(err, "Materialize error")
		c.Nil(tool, "Materialize tool")
	})
	t.Run("declines when only Agents is set", func(t *testing.T) {
		c := assert.NewAborting(t)
		// A user caller has a spawner (Agents) but no PyModuleStarter: the
		// delegate verbs of the pymodule family are child-token surfaces.
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{Agents: &recordingSpawner{}})
		c.NoError(err, "Materialize error")
		c.Nil(tool, "Agents alone must not materialize pymodule_start; got")
	})
	t.Run("binds the gate's own spawner", func(t *testing.T) {
		c := assert.NewCollecting(t)
		sp := &recordingSpawner{}
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{PyModuleStarter: sp})
		c.Require().NoError(err, "Materialize error =")
		c.Require().NotNil(tool, "Materialize tool = nil, want a pymodule_start tool")
		out, err := tool.Execute(context.Background(), tools.ToolInput(`{"repo":"local","script":"driver","labels":{"env":"work"}}`))
		c.Require().NoError(err, "Execute error =")
		c.Require().Len(sp.specs, 1, "want 1 spawn, got %d", len(sp.specs))
		spec := sp.specs[0]
		c.Require().False(spec.Kind != protocol.KindScript || spec.Script == nil || spec.Script.Script != "driver", "spawn spec = %+v, want a kind=script spec for driver", spec)
		c.Eq("work", spec.Labels["env"], "labels = %v, want env=work", spec.Labels)
		c.StrContains(out.Text, "c_new", "result")
	})
}

// The face description must never promise the settlement notification the
// face cannot reliably deliver — the registry text's promise is the fundi
// surface's, and the face-local wording replaces it wholesale.
func TestMCPPyModuleStartDescriptionCarriesNoSettlementPromise(t *testing.T) {
	c := assert.NewCollecting(t)
	d := mcpPyModuleStartBlueprint{}.Description()
	c.NotStrContains(d, "you are notified when it settles", "face description promises a settlement notification; the face can only push best-effort log messages")
	for _, want := range []string{"agent_list", "pymodule_run"} {
		c.StrContains(d, want, "face description lost")
	}
}
