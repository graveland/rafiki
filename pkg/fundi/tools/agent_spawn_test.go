// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

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

// TestAgentSpawnSandboxRequiresScope pins the brief's rule that `scope` is
// required inside the sandbox block: an omitted scope is an error, never a
// default, exactly as an omitted mount kind is.
func TestAgentSpawnSandboxRequiresScope(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)

	for _, in := range []string{
		`{"prompt":"x","sandbox":{"image":"img:1"}}`,
		`{"prompt":"x","sandbox":{"scope":""}}`,
	} {
		_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(in))
		if err == nil {
			t.Errorf("%s: want an error for a missing scope, got nil", in)
			continue
		}
		c.StrContains(err.Error(), "sandbox.scope is required", "missing scope")
	}

	_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(`{"prompt":"x","sandbox":{"scope":"all"}}`))
	if err == nil {
		t.Fatal("want an error for an unknown scope")
	}
	c.StrContains(err.Error(), "sandbox.scope must be", "unknown scope")
	c.Empty(sp.spawned, "a refused sandbox block must not spawn, got %d", len(sp.spawned))
}

// TestAgentSpawnSandboxReachesSpawner pins that a valid sandbox block is parsed
// at the tool and reaches SpawnSpec.Sandbox verbatim (the field the daemon maps
// onto SpawnRequest.sandbox), with the same flat typed fields sandbox_create
// takes minus name/ttl.
func TestAgentSpawnSandboxReachesSpawner(t *testing.T) {
	c := assert.NewAborting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)

	in := `{"prompt":"do it","sandbox":{"scope":"subtree","image":"rafiki/sandbox:1",` +
		`"launcher":"greyshift","workdir":"/work","network":"none","memory_bytes":1024,` +
		`"cpus":1.5,"pids_limit":64,` +
		`"mounts":[{"target":"/work","kind":"ro","host_path":"/srv/repos/a"},` +
		`{"target":"/scratch","kind":"rw"}]}}`
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(in)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	c.Require().Len(sp.spawned, 1, "want 1 spawn, got %d", len(sp.spawned))
	sb := sp.spawned[0].Sandbox
	c.Require().NotNil(sb, "SpawnSpec.Sandbox is nil")
	c.Eq(protocol.ScopeSubtree, sb.Scope, "Scope")
	c.Eq("rafiki/sandbox:1", sb.Image, "Image")
	c.Eq("greyshift", sb.Launcher, "Launcher")
	c.Eq("/work", sb.Workdir, "Workdir")
	c.Eq(protocol.NetworkNone, sb.Network, "Network")
	c.Eq(int64(1024), sb.MemoryBytes, "MemoryBytes")
	c.Eq(1.5, sb.CPUs, "CPUs")
	c.Eq(int64(64), sb.PidsLimit, "PidsLimit")
	c.Eq("", sb.Name, "a spawn-block sandbox carries no Name")
	c.Eq(time.Duration(0), sb.TTL, "a spawn-block sandbox carries no TTL")
	c.Len(sb.Mounts, 2, "Mounts")
	c.EqDeep(protocol.SandboxMount{Target: "/work", Kind: protocol.MountRO, HostPath: "/srv/repos/a"}, sb.Mounts[0], "mount 0")
	c.EqDeep(protocol.SandboxMount{Target: "/scratch", Kind: protocol.MountRW}, sb.Mounts[1], "mount 1")

	// An absent block leaves SpawnSpec.Sandbox nil — no sandbox, not an empty one.
	sp2 := &fakeSpawner{}
	reg2, ctx2 := newAgentTools(t, sp2)
	if _, err := reg2.Execute(ctx2, "agent_spawn", json.RawMessage(`{"prompt":"x"}`)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	c.Nil(sp2.spawned[0].Sandbox, "absent sandbox block arrived as %#v, want nil", sp2.spawned[0].Sandbox)
}

// TestAgentSpawnSandboxSchemaRequiresScope pins the schema half: the sandbox
// property requires scope, and its mounts items require target and kind with the
// same kind enum sandbox_create uses.
func TestAgentSpawnSandboxSchemaRequiresScope(t *testing.T) {
	c := assert.NewCollecting(t)
	schema := sandboxSchemaJSON(t, &AgentSpawnBlueprint{})
	props := schema["properties"].(map[string]any)
	sandbox, ok := props["sandbox"].(map[string]any)
	c.Require().True(ok, "agent_spawn has no sandbox property")
	c.EqDiff([]string{"scope"}, toStrings(sandbox["required"].([]any)), "sandbox required")

	sbProps := sandbox["properties"].(map[string]any)
	scope := sbProps["scope"].(map[string]any)
	c.EqDiff([]string{"self", "subtree"}, toStrings(scope["enum"].([]any)), "scope enum")
	items := sbProps["mounts"].(map[string]any)["items"].(map[string]any)
	c.EqDiff([]string{"target", "kind"}, toStrings(items["required"].([]any)), "sandbox mounts items required")
	kind := items["properties"].(map[string]any)["kind"].(map[string]any)
	c.EqDiff([]string{"ro", "rw", "ephemeral"}, toStrings(kind["enum"].([]any)), "sandbox mount kind enum")
	network := sbProps["network"].(map[string]any)
	c.EqDiff([]string{"egress", "none"}, toStrings(network["enum"].([]any)), "sandbox network enum")
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
