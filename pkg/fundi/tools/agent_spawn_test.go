// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// agent_spawn's new preset/narrowing fields must arrive at the spawner
// verbatim, with the tri-state intact: "tools": [] is a non-nil empty slice
// (a request for none), an absent field is nil (no request).
func TestAgentSpawnCopiesPresetFields(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	in := `{"prompt":"do it","preset":"default:implementer","thinking":"high",` +
		`"append_system_prompt":"be terse","tools":[],"skills":["x"],"context_files":false}`
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(in)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	c.Require().Len(sp.spawned, 1, "want 1 spawn, got %d", len(sp.spawned))
	spec := sp.spawned[0]
	c.Eq("default:implementer", spec.Preset, "Preset =")
	c.Eq("high", spec.Thinking, "Thinking =")
	c.Eq("be terse", spec.AppendSystemPrompt, "AppendSystemPrompt =")
	c.False(spec.Tools == nil || len(*spec.Tools) != 0, `"tools":[] arrived as %#v, want a non-nil pointer to an empty slice`, spec.Tools)
	c.False(spec.Skills == nil || len(*spec.Skills) != 1 || (*spec.Skills)[0] != "x", "Skills arrived as %#v, want [x]", spec.Skills)
	c.Nil(spec.MCPServers, "absent mcp_servers arrived as")
	c.False(spec.ContextFiles == nil || *spec.ContextFiles != false, `"context_files":false arrived as %#v, want a pointer to false`, spec.ContextFiles)

	// A spawn without any of the fields must arrive with none of them set:
	// absent means no request, not a default.
	sp2 := &fakeSpawner{}
	reg2, ctx2 := newAgentTools(t, sp2)
	_, err := reg2.Execute(ctx2, "agent_spawn", json.RawMessage(`{"prompt":"x"}`))
	c.Require().NoError(err, "agent_spawn")
	got := sp2.spawned[0]
	c.False(got.Preset != "" || got.Thinking != "" || got.AppendSystemPrompt != "", "absent string fields arrived as %q/%q/%q, want all empty", got.Preset, got.Thinking, got.AppendSystemPrompt)
	c.False(got.Tools != nil || got.Skills != nil || got.MCPServers != nil || got.ContextFiles != nil, "absent pointer fields arrived as %#v/%#v/%#v/%#v, want all nil", got.Tools, got.Skills, got.MCPServers, got.ContextFiles)
}

// The MCP face excises the span between "You will be notified" and "Keep
// doing your own work" from this description. If either marker moves or
// vanishes, that excision silently slices different text — so their presence
// and order are pinned.
func TestAgentSpawnDescriptionKeepsExcisionMarkers(t *testing.T) {
	c := assert.NewCollecting(t)
	d := agentSpawnDescription
	notif := strings.Index(d, "You will be notified")
	keep := strings.Index(d, "Keep doing your own work")
	c.Require().False(notif < 0 || keep < 0, "agent_spawn description lost an excision marker: \"You will be notified\" at %d, \"Keep doing your own work\" at %d", notif, keep)
	c.LessOrEqual(keep, notif, "excision markers out of order: \"You will be notified\" at %d, \"Keep doing your own work\" at", notif)
}

// TestAgentSpawnPrefillParsed pins that agent_spawn parses its prefill list
// at the tool (a malformed entry fails the call instead of failing inside
// the child) and that the parsed entries reach SpawnSpec.Prefill verbatim.
func TestAgentSpawnPrefillParsed(t *testing.T) {
	t.Run("entries reach SpawnSpec", func(t *testing.T) {
		c := assert.NewAborting(t)
		sp := &fakeSpawner{}
		reg, ctx := newAgentTools(t, sp)
		in := `{"prompt":"do it","prefill":["CLAUDE.md","pkg/prefill/prefill.go:10-40","src/**/*.rs"]}`
		_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(in))
		c.NoError(err, "agent_spawn")
		c.Len(sp.spawned, 1, "want 1 spawn, got %d", len(sp.spawned))
		spec := sp.spawned[0]
		want := []struct {
			path       string
			start, end int
		}{
			{"CLAUDE.md", 0, 0},
			{"pkg/prefill/prefill.go", 10, 40},
			{"src/**/*.rs", 0, 0},
		}
		c.Len(spec.Prefill, len(want), "Prefill")
		for i, w := range want {
			if spec.Prefill[i].Path != w.path || spec.Prefill[i].Start != w.start || spec.Prefill[i].End != w.end {
				t.Errorf("Prefill[%d] = %+v, want %s:%d-%d", i, spec.Prefill[i], w.path, w.start, w.end)
			}
		}
	})

	t.Run("bad range errors without spawning", func(t *testing.T) {
		c := assert.NewCollecting(t)
		sp := &fakeSpawner{}
		reg, ctx := newAgentTools(t, sp)
		_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(`{"prompt":"x","prefill":["f.go:40-10"]}`))
		c.Require().Error(err, "want an error for a bad range, got nil")
		if !strings.HasPrefix(err.Error(), "agent_spawn: prefill: ") {
			t.Errorf("error = %q, want it wrapped as agent_spawn: prefill: …", err)
		}
		c.Empty(sp.spawned, "a failed parse must not spawn, got %d spawns", len(sp.spawned))
	})
}
