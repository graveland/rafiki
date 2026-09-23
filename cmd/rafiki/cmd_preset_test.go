package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/presets"
)

// TestPresetPutNameMismatch pins put's guard: a spec that names itself must
// be saved under the same name, or the file and the argument disagree about
// what is being written.
func TestPresetPutNameMismatch(t *testing.T) {
	_, err := presetPutRequest("argname", []byte(`{"name":"filename"}`))
	if err == nil {
		t.Fatal("presetPutRequest with a mismatched name = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), `name in file "filename" does not match "argname"`) {
		t.Errorf("error = %v, want the mismatch named with both names", err)
	}
}

// TestPresetPutParsesTriState pins the tri-state allowlist handling in put's
// file-to-request step: `[]` must survive as a NON-nil empty Tools ("none"),
// absent must stay nil ("the kind's default"). Collapsing the two would turn
// "all tools" into "no tools" on a plain save.
func TestPresetPutParsesTriState(t *testing.T) {
	req, err := presetPutRequest("strict", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatalf("presetPutRequest(tools:[]): %v", err)
	}
	if req.Preset.Tools == nil {
		t.Fatal("Tools = nil for {\"tools\":[]}, want a non-nil empty StringList (none)")
	}
	if len(req.Preset.Tools.Items) != 0 {
		t.Errorf("Tools.Items = %v, want empty", req.Preset.Tools.Items)
	}

	req, err = presetPutRequest("bare", []byte(`{}`))
	if err != nil {
		t.Fatalf("presetPutRequest({}): %v", err)
	}
	if req.Preset.Tools != nil {
		t.Errorf("Tools = %v for {}, want nil (the kind's default)", req.Preset.Tools)
	}
}

// TestPresetViewJSONShape pins the JSON shape a get/list prints: the spec's
// fields flatten to the TOP LEVEL next to the version metadata — "model" as
// a sibling of "version", not nested under a "spec" key.
func TestPresetViewJSONShape(t *testing.T) {
	rec := presets.Record{Name: "reviewer", Kind: presets.KindFundi, Model: "anthropic/claude-sonnet-4-5"}
	rec.CreatedAt = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	view := presetView{
		Version:   3,
		CreatedAt: rec.CreatedAt,
		Spec:      presets.SpecOf(rec),
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal presetView: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["version"].(float64) != 3 {
		t.Errorf("version = %v, want 3", m["version"])
	}
	if m["model"] != "anthropic/claude-sonnet-4-5" {
		t.Errorf("model = %v, want the spec's model flattened to the top level", m["model"])
	}
	if m["name"] != "reviewer" {
		t.Errorf("name = %v, want the spec's name flattened to the top level", m["name"])
	}
	if m["kind"] != presets.KindFundi {
		t.Errorf("kind = %v, want fundi", m["kind"])
	}
}

// TestPresetViewDeletedAt pins the omitempty on deleted_at: a live preset
// must not print a deleted_at key at all.
func TestPresetViewDeletedAt(t *testing.T) {
	rec := presets.Record{Name: "x", Kind: presets.KindFundi}
	b, err := json.Marshal(presetView{Spec: presets.SpecOf(rec)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "deleted_at") {
		t.Errorf("live preset view carries deleted_at: %s", b)
	}
}
