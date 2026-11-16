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
	_, err := presetPutRequest("argname", []byte(`{"name":"filename"}`), nil)
	c.Require().Error(err, "presetPutRequest with a mismatched name = nil error, want a failure")
	c.StrContains(err.Error(), `name in file "filename" does not match "argname"`, "error = %v, want the mismatch named with both names", err)
}

// TestPresetPutParsesTriState pins the tri-state allowlist handling in put's
// file-to-request step: `[]` must survive as a NON-nil empty Tools ("none"),
// absent must stay nil ("the kind's default"). Collapsing the two would turn
// "all tools" into "no tools" on a plain save.
func TestPresetPutParsesTriState(t *testing.T) {
	c := assert.NewCollecting(t)
	req, err := presetPutRequest("strict", []byte(`{"tools":[]}`), nil)
	c.Require().NoError(err, "presetPutRequest(tools:[])")
	c.Require().NotNil(req.Preset.Tools, "Tools = nil for {\"tools\":[]}, want a non-nil empty StringList (none)")
	c.Empty(req.Preset.Tools.Items, "Tools.Items")

	req, err = presetPutRequest("bare", []byte(`{}`), nil)
	c.Require().NoError(err, "presetPutRequest({})")
	c.Nil(req.Preset.Tools, "Tools")
}

// TestPresetPutWorkspaceTools pins --workspace-tools on `preset put`: the flag
// overrides the file's value, and leaving it off keeps the file's — including
// "native", which on a PRESET is a real pin (the built-ins stay) and not a
// synonym for unset. Folding it into "" would turn a pin into "the spawn's
// choice", the opposite of what the caller wrote.
func TestPresetPutWorkspaceTools(t *testing.T) {
	c := assert.NewCollecting(t)

	// The flag overrides the file, whichever way the file points.
	rafiki := "rafiki"
	native := "native"
	for _, tc := range []struct {
		doc  string
		flag *string
		want *string
	}{
		{`{"kind":"claude"}`, &rafiki, &rafiki},
		{`{"kind":"claude","workspace_tools":"native"}`, &rafiki, &rafiki},
		{`{"kind":"claude","workspace_tools":"rafiki"}`, &native, &native},
		{`{"kind":"claude","workspace_tools":"native"}`, nil, &native},
		{`{"kind":"claude"}`, nil, nil},
	} {
		req, err := presetPutRequest("claude-seat", []byte(tc.doc), tc.flag)
		c.Require().NoError(err, "presetPutRequest(%s, flag=%v)", tc.doc, tc.flag)
		if tc.want == nil {
			c.Eq("", req.Preset.GetWorkspaceTools(), "unset stays unset for %s", tc.doc)
			continue
		}
		c.Eq(*tc.want, req.Preset.GetWorkspaceTools(), "workspace_tools for %s, flag=%v", tc.doc, tc.flag)
	}
}

// TestPresetPutWorkspaceToolsFlag pins the flag's own parsing: only the two
// real modes are accepted, judged by exact match rather than through
// protocol.NormalizeWorkspaceTools — that helper folds "native" into the empty
// string, which for a preset is a different value.
func TestPresetPutWorkspaceToolsFlag(t *testing.T) {
	c := assert.NewCollecting(t)

	cmd := newPresetPutCmd()
	got, err := presetPutWorkspaceToolsFlag(cmd)
	c.Require().NoError(err, "no flag")
	c.Nil(got, "an unset flag must not override the file")

	for _, mode := range []string{"native", "rafiki"} {
		cmd = newPresetPutCmd()
		c.Require().NoError(cmd.Flags().Set("workspace-tools", mode), "--workspace-tools %s", mode)
		got, err = presetPutWorkspaceToolsFlag(cmd)
		c.Require().NoError(err, "--workspace-tools %s", mode)
		c.Require().NotNil(got, "--workspace-tools %s", mode)
		c.Eq(mode, *got, "--workspace-tools %s", mode)
	}

	cmd = newPresetPutCmd()
	c.Require().NoError(cmd.Flags().Set("workspace-tools", "none"))
	_, err = presetPutWorkspaceToolsFlag(cmd)
	c.Require().Error(err, "--workspace-tools none = nil error")
	c.StrContains(err.Error(), `"native"`, "the refusal names the accepted values: %v", err)
}

// TestPresetGetShowsWorkspaceTools pins that `preset get` renders the field:
// the table row appears only for a SET value, and "native" prints as itself.
func TestPresetGetShowsWorkspaceTools(t *testing.T) {
	c := assert.NewCollecting(t)
	native := "native"
	set := presets.Record{Name: "claude-seat", Kind: presets.KindClaude, WorkspaceTools: &native}
	cells := presetSpecCells(presetViewOf(presets.ToProto(set)))
	found := false
	for _, cell := range cells {
		if cell[0] == "workspace_tools" {
			found = true
			c.Eq("native", cell[1], "workspace_tools cell")
		}
	}
	c.True(found, "preset get does not show a set workspace_tools: %v", cells)

	unset := presets.Record{Name: "claude-seat", Kind: presets.KindClaude}
	for _, cell := range presetSpecCells(presetViewOf(presets.ToProto(unset))) {
		c.NotEq("workspace_tools", cell[0], "an unset workspace_tools must not print a row")
	}
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
