// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// presetBool avoids a package-wide helper name a future test might also want;
// the ContextFiles table is the only place that needs a *bool literal.
func presetBool(v bool) *bool { return &v }

// TestPresetWiringShapingTriState pins applySpawnSpecShaping's tri-state
// contract end to end: a nil list is "no request" (nothing on the request
// changes), a non-nil empty list is "none" (the off flag), and a non-empty
// list is "exactly those" — never collapsed. ContextFiles is the same shape
// with a bool: non-nil false means skip, true and nil mean nothing.
func TestPresetWiringShapingTriState(t *testing.T) {
	t.Run("scalars are copied", func(t *testing.T) {
		var req protocol.SpawnRequest
		applySpawnSpecShaping(&req, tools.SpawnSpec{
			Preset:             "review",
			Thinking:           "high",
			AppendSystemPrompt: "be terse",
		})
		if req.Preset != "review" || req.Thinking != "high" || req.AppendSystemPrompt != "be terse" {
			t.Fatalf("scalar fields not copied: %+v", req)
		}
	})

	for _, tc := range []struct {
		name string
		set  func(s *tools.SpawnSpec, v *[]string)
		off  func(r *protocol.SpawnRequest) bool
		got  func(r *protocol.SpawnRequest) []string
	}{
		{"tools", func(s *tools.SpawnSpec, v *[]string) { s.Tools = v },
			func(r *protocol.SpawnRequest) bool { return r.NoBuiltinTools },
			func(r *protocol.SpawnRequest) []string {
				if r.Tools == "" {
					return nil
				}
				return strings.Split(r.Tools, ",")
			}},
		{"skills", func(s *tools.SpawnSpec, v *[]string) { s.Skills = v },
			func(r *protocol.SpawnRequest) bool { return r.NoSkills },
			func(r *protocol.SpawnRequest) []string { return r.Skills }},
		{"mcp_servers", func(s *tools.SpawnSpec, v *[]string) { s.MCPServers = v },
			func(r *protocol.SpawnRequest) bool { return r.NoMCP },
			func(r *protocol.SpawnRequest) []string { return r.MCPServers }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("nil means no request", func(t *testing.T) {
				var spec tools.SpawnSpec
				tc.set(&spec, nil)
				var req protocol.SpawnRequest
				applySpawnSpecShaping(&req, spec)
				if tc.off(&req) {
					t.Errorf("%s: off flag set for a nil request", tc.name)
				}
				if got := tc.got(&req); len(got) != 0 {
					t.Errorf("%s: list set for a nil request: %v", tc.name, got)
				}
			})
			t.Run("empty means none", func(t *testing.T) {
				empty := []string{}
				var spec tools.SpawnSpec
				tc.set(&spec, &empty)
				var req protocol.SpawnRequest
				applySpawnSpecShaping(&req, spec)
				if !tc.off(&req) {
					t.Errorf("%s: off flag not set for a non-nil empty list", tc.name)
				}
			})
			t.Run("list means exactly those", func(t *testing.T) {
				list := []string{"alpha", "beta"}
				var spec tools.SpawnSpec
				tc.set(&spec, &list)
				var req protocol.SpawnRequest
				applySpawnSpecShaping(&req, spec)
				if tc.off(&req) {
					t.Errorf("%s: off flag set for a non-empty list", tc.name)
				}
				got := tc.got(&req)
				if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
					t.Errorf("%s: got %v, want [alpha beta]", tc.name, got)
				}
			})
		})
	}

	t.Run("context_files", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   *bool
			want bool
		}{
			{"nil means no request", nil, false},
			{"non-nil false means skip them", presetBool(false), true},
			{"true sets nothing", presetBool(true), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var req protocol.SpawnRequest
				applySpawnSpecShaping(&req, tools.SpawnSpec{ContextFiles: tc.in})
				if req.NoContextFiles != tc.want {
					t.Errorf("NoContextFiles = %v, want %v", req.NoContextFiles, tc.want)
				}
			})
		}
	})
}

// TestPresetWiringSpawnersLeaveKindEmptyWithPreset pins the kind decision both
// spawners share: the fundi default applies only to a preset-less spawn, and a
// preset leaves an empty kind empty for the controller's applyPreset to take
// from the preset (an explicit kind rides along and is still conflict-checked
// there).
func TestPresetWiringSpawnersLeaveKindEmptyWithPreset(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec tools.SpawnSpec
		want string
	}{
		{"no preset and no kind defaults to fundi", tools.SpawnSpec{}, protocol.KindFundi},
		{"a preset leaves an empty kind empty", tools.SpawnSpec{Preset: "x"}, ""},
		{"a preset with an explicit kind keeps it", tools.SpawnSpec{Kind: "claude", Preset: "x"}, "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spawnKind(tc.spec); got != tc.want {
				t.Fatalf("spawnKind(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}
