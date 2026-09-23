// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// agent_spawn's new preset/narrowing fields must arrive at the spawner
// verbatim, with the tri-state intact: "tools": [] is a non-nil empty slice
// (a request for none), an absent field is nil (no request).
func TestAgentSpawnCopiesPresetFields(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	in := `{"prompt":"do it","preset":"default:implementer","thinking":"high",` +
		`"append_system_prompt":"be terse","tools":[],"skills":["x"],"context_files":false}`
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(in)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	if len(sp.spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(sp.spawned))
	}
	spec := sp.spawned[0]
	if spec.Preset != "default:implementer" {
		t.Errorf("Preset = %q", spec.Preset)
	}
	if spec.Thinking != "high" {
		t.Errorf("Thinking = %q", spec.Thinking)
	}
	if spec.AppendSystemPrompt != "be terse" {
		t.Errorf("AppendSystemPrompt = %q", spec.AppendSystemPrompt)
	}
	if spec.Tools == nil || len(*spec.Tools) != 0 {
		t.Errorf(`"tools":[] arrived as %#v, want a non-nil pointer to an empty slice`, spec.Tools)
	}
	if spec.Skills == nil || len(*spec.Skills) != 1 || (*spec.Skills)[0] != "x" {
		t.Errorf("Skills arrived as %#v, want [x]", spec.Skills)
	}
	if spec.MCPServers != nil {
		t.Errorf("absent mcp_servers arrived as %#v, want nil", spec.MCPServers)
	}
	if spec.ContextFiles == nil || *spec.ContextFiles != false {
		t.Errorf(`"context_files":false arrived as %#v, want a pointer to false`, spec.ContextFiles)
	}

	// A spawn without any of the fields must arrive with none of them set:
	// absent means no request, not a default.
	sp2 := &fakeSpawner{}
	reg2, ctx2 := newAgentTools(t, sp2)
	if _, err := reg2.Execute(ctx2, "agent_spawn", json.RawMessage(`{"prompt":"x"}`)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	got := sp2.spawned[0]
	if got.Preset != "" || got.Thinking != "" || got.AppendSystemPrompt != "" {
		t.Errorf("absent string fields arrived as %q/%q/%q, want all empty", got.Preset, got.Thinking, got.AppendSystemPrompt)
	}
	if got.Tools != nil || got.Skills != nil || got.MCPServers != nil || got.ContextFiles != nil {
		t.Errorf("absent pointer fields arrived as %#v/%#v/%#v/%#v, want all nil",
			got.Tools, got.Skills, got.MCPServers, got.ContextFiles)
	}
}

// The MCP face excises the span between "You will be notified" and "Keep
// doing your own work" from this description. If either marker moves or
// vanishes, that excision silently slices different text — so their presence
// and order are pinned.
func TestAgentSpawnDescriptionKeepsExcisionMarkers(t *testing.T) {
	d := agentSpawnDescription
	notif := strings.Index(d, "You will be notified")
	keep := strings.Index(d, "Keep doing your own work")
	if notif < 0 || keep < 0 {
		t.Fatalf("agent_spawn description lost an excision marker: \"You will be notified\" at %d, \"Keep doing your own work\" at %d", notif, keep)
	}
	if notif > keep {
		t.Errorf("excision markers out of order: \"You will be notified\" at %d, \"Keep doing your own work\" at %d", notif, keep)
	}
}
