// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"slices"
	"testing"
	"time"
)

// TestPresetProtoRoundTripTriState pins the tri-state through the wire:
// for each of tools/skills/mcp_servers, nil / empty / {"a"} survive
// FromProto(ToProto(r)) with nil-ness preserved — nil = the kind's default,
// non-nil empty = "none". It also pins the optional scalars (ContextFiles,
// MaxCost nil vs 0, MaxDepth, MaxChildren), Labels, and the RFC3339 times.
func TestPresetProtoRoundTripTriState(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deleted := created.Add(30 * time.Minute)

	for _, tc := range []struct {
		name   string
		record Record
	}{
		{
			name:   "unset everywhere",
			record: Record{ID: 1, Name: "a", Kind: KindFundi, CreatedAt: created},
		},
		{
			name: "empty means none",
			record: Record{
				ID: 2, Name: "b", Kind: KindFundi, Labels: map[string]string{},
				Tools: []string{}, Skills: []string{}, MCPServers: []string{},
				ContextFiles: ptr(false),
				MaxCost:      ptr(0.0), MaxDepth: ptr(0), MaxChildren: ptr(0),
				CreatedAt: created, DeletedAt: &deleted,
			},
		},
		{
			name: "fully set",
			record: Record{
				ID: 3, Name: "c:d", Kind: KindClaude, Provider: "p", Model: "m",
				Thinking: "high", Executor: "env=work",
				Labels:       map[string]string{"team": "core"},
				Tools:        []string{"a"},
				Skills:       []string{"b"},
				MCPServers:   []string{"c"},
				ContextFiles: ptr(false),
				SystemPrompt: "sys", AppendSystemPrompt: "append",
				MaxCost: ptr(1.5), MaxDepth: ptr(2), MaxChildren: ptr(3),
				WrittenByChild: "c_child", DeletedAt: &deleted, CreatedAt: created,
			},
		},
	} {
		got := FromProto(ToProto(tc.record))

		for _, f := range []struct {
			name       string
			want, gotF []string
		}{
			{"tools", tc.record.Tools, got.Tools},
			{"skills", tc.record.Skills, got.Skills},
			{"mcp_servers", tc.record.MCPServers, got.MCPServers},
		} {
			if msg := triStateDiff(f.want, f.gotF); msg != "" {
				t.Errorf("%s: %s tri-state broken (%s): want %#v, got %#v", tc.name, f.name, msg, f.want, f.gotF)
			}
		}

		assertPtrEqual(t, tc.name+"/context_files", tc.record.ContextFiles, got.ContextFiles)
		assertPtrEqual(t, tc.name+"/max_cost", tc.record.MaxCost, got.MaxCost)
		assertPtrEqual(t, tc.name+"/max_depth", tc.record.MaxDepth, got.MaxDepth)
		assertPtrEqual(t, tc.name+"/max_children", tc.record.MaxChildren, got.MaxChildren)

		if (tc.record.Labels == nil) != (got.Labels == nil) {
			t.Errorf("%s: labels nil-ness changed: want nil=%v, got nil=%v", tc.name, tc.record.Labels == nil, got.Labels == nil)
		} else if len(tc.record.Labels) != len(got.Labels) {
			t.Errorf("%s: labels = %v, want %v", tc.name, got.Labels, tc.record.Labels)
		} else {
			for k, v := range tc.record.Labels {
				if got.Labels[k] != v {
					t.Errorf("%s: labels[%q] = %q, want %q", tc.name, k, got.Labels[k], v)
				}
			}
		}

		for _, f := range []struct {
			name       string
			want, gotF string
		}{
			{"name", tc.record.Name, got.Name},
			{"description", tc.record.Description, got.Description},
			{"kind", tc.record.Kind, got.Kind},
			{"provider", tc.record.Provider, got.Provider},
			{"model", tc.record.Model, got.Model},
			{"thinking", tc.record.Thinking, got.Thinking},
			{"executor", tc.record.Executor, got.Executor},
			{"system_prompt", tc.record.SystemPrompt, got.SystemPrompt},
			{"append_system_prompt", tc.record.AppendSystemPrompt, got.AppendSystemPrompt},
			{"written_by_child", tc.record.WrittenByChild, got.WrittenByChild},
		} {
			if f.want != f.gotF {
				t.Errorf("%s: %s = %q, want %q", tc.name, f.name, f.gotF, f.want)
			}
		}

		if got.ID != tc.record.ID {
			t.Errorf("%s: version = %d, want %d", tc.name, got.ID, tc.record.ID)
		}
		if !got.CreatedAt.Equal(tc.record.CreatedAt) {
			t.Errorf("%s: created_at = %v, want %v", tc.name, got.CreatedAt, tc.record.CreatedAt)
		}
		if (tc.record.DeletedAt == nil) != (got.DeletedAt == nil) {
			t.Errorf("%s: deleted_at nil-ness changed", tc.name)
		} else if tc.record.DeletedAt != nil && !got.DeletedAt.Equal(*tc.record.DeletedAt) {
			t.Errorf("%s: deleted_at = %v, want %v", tc.name, got.DeletedAt, *tc.record.DeletedAt)
		}
	}
}

// triStateDiff describes how got lost the tri-state of want, or "" when the
// nil-ness and contents survived. The assertions it makes are exactly
// "== nil" and "!= nil && len == 0".
func triStateDiff(want, got []string) string {
	switch {
	case want == nil && got == nil:
		return ""
	case want == nil:
		return "nil became non-nil"
	case got == nil:
		return "non-nil became nil"
	case len(want) == 0 && len(got) == 0:
		return "" // both non-nil empty: "none" preserved
	case len(want) == 0:
		return "empty gained items"
	case len(got) == 0:
		return "list lost its items"
	case !slices.Equal(want, got):
		return "items differ"
	}
	return ""
}

func assertPtrEqual[T comparable](t *testing.T, name string, want, got *T) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Errorf("%s: nil-ness changed: want nil=%v, got nil=%v", name, want == nil, got == nil)
		return
	}
	if want == nil {
		return
	}
	if *want != *got {
		t.Errorf("%s: got %v, want %v", name, *got, *want)
	}
}

func ptr[T any](v T) *T { return &v }
