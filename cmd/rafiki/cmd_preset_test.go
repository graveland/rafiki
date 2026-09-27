package main

import (
	"encoding/json"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/presets"

	"github.com/multigres/testkit/assert"
)

// TestPresetPutNameMismatch pins put's guard: a spec that names itself must
// be saved under the same name, or the file and the argument disagree about
// what is being written.
func TestPresetPutNameMismatch(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := presetPutRequest("argname", []byte(`{"name":"filename"}`))
	c.Require().Error(err, "presetPutRequest with a mismatched name = nil error, want a failure")
	c.StrContains(err.Error(), `name in file "filename" does not match "argname"`, "error = %v, want the mismatch named with both names", err)
}

// TestPresetPutParsesTriState pins the tri-state allowlist handling in put's
// file-to-request step: `[]` must survive as a NON-nil empty Tools ("none"),
// absent must stay nil ("the kind's default"). Collapsing the two would turn
// "all tools" into "no tools" on a plain save.
func TestPresetPutParsesTriState(t *testing.T) {
	c := assert.NewCollecting(t)
	req, err := presetPutRequest("strict", []byte(`{"tools":[]}`))
	c.Require().NoError(err, "presetPutRequest(tools:[])")
	c.Require().NotNil(req.Preset.Tools, "Tools = nil for {\"tools\":[]}, want a non-nil empty StringList (none)")
	c.Empty(req.Preset.Tools.Items, "Tools.Items")

	req, err = presetPutRequest("bare", []byte(`{}`))
	c.Require().NoError(err, "presetPutRequest({})")
	c.Nil(req.Preset.Tools, "Tools")
}

// TestPresetViewJSONShape pins the JSON shape a get/list prints: the spec's
// fields flatten to the TOP LEVEL next to the version metadata — "model" as
// a sibling of "version", not nested under a "spec" key.
func TestPresetViewJSONShape(t *testing.T) {
	c := assert.NewCollecting(t)
	rec := presets.Record{Name: "reviewer", Kind: presets.KindFundi, Model: "anthropic/claude-sonnet-4-5"}
	rec.CreatedAt = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	view := presetView{
		Version:   3,
		CreatedAt: rec.CreatedAt,
		Spec:      presets.SpecOf(rec),
	}
	b, err := json.Marshal(view)
	c.Require().NoError(err, "marshal presetView")
	var m map[string]any
	c.Require().NoError(json.Unmarshal(b, &m), "decode")
	c.Eq(3, m["version"].(float64), "version = %v, want 3", m["version"])
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
	c := assert.NewCollecting(t)
	rec := presets.Record{Name: "x", Kind: presets.KindFundi}
	b, err := json.Marshal(presetView{Spec: presets.SpecOf(rec)})
	c.Require().NoError(err)
	c.NotStrContains(string(b), "deleted_at", "live preset view carries deleted_at: %s", b)
}
