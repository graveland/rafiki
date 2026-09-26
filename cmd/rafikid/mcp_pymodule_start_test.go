// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
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
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{})
		if err != nil {
			t.Fatalf("Materialize error = %v, want nil", err)
		}
		if tool != nil {
			t.Fatalf("Materialize tool = %v, want nil (the interactive human gets `rafiki create --kind script`)", tool)
		}
	})
	t.Run("declines when only Agents is set", func(t *testing.T) {
		// A user caller has a spawner (Agents) but no PyModuleStarter: the
		// delegate verbs of the pymodule family are child-token surfaces.
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{Agents: &recordingSpawner{}})
		if err != nil {
			t.Fatalf("Materialize error = %v, want nil", err)
		}
		if tool != nil {
			t.Fatalf("Agents alone must not materialize pymodule_start; got %v", tool)
		}
	})
	t.Run("binds the gate's own spawner", func(t *testing.T) {
		sp := &recordingSpawner{}
		tool, err := mcpPyModuleStartBlueprint{}.Materialize(tools.ToolOpts{PyModuleStarter: sp})
		if err != nil {
			t.Fatalf("Materialize error = %v", err)
		}
		if tool == nil {
			t.Fatal("Materialize tool = nil, want a pymodule_start tool")
		}
		out, err := tool.Execute(context.Background(), tools.ToolInput(`{"repo":"local","script":"driver","labels":{"env":"work"}}`))
		if err != nil {
			t.Fatalf("Execute error = %v", err)
		}
		if len(sp.specs) != 1 {
			t.Fatalf("want 1 spawn, got %d", len(sp.specs))
		}
		spec := sp.specs[0]
		if spec.Kind != protocol.KindScript || spec.Script == nil || spec.Script.Script != "driver" {
			t.Fatalf("spawn spec = %+v, want a kind=script spec for driver", spec)
		}
		if spec.Labels["env"] != "work" {
			t.Errorf("labels = %v, want env=work", spec.Labels)
		}
		if !strings.Contains(out.Text, "c_new") {
			t.Errorf("result %q does not name the child", out.Text)
		}
	})
}

// The face description must never promise the settlement notification the
// face cannot reliably deliver — the registry text's promise is the fundi
// surface's, and the face-local wording replaces it wholesale.
func TestMCPPyModuleStartDescriptionCarriesNoSettlementPromise(t *testing.T) {
	d := mcpPyModuleStartBlueprint{}.Description()
	if strings.Contains(d, "you are notified when it settles") {
		t.Errorf("face description promises a settlement notification; the face can only push best-effort log messages")
	}
	for _, want := range []string{"agent_list", "pymodule_run"} {
		if !strings.Contains(d, want) {
			t.Errorf("face description lost %q; the deliberate-check and run/start pairing are what the surface teaches", want)
		}
	}
}
