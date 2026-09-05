package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── renderPresets ────────────────────────────────────────────────────────────

func TestRenderPresets_JSON(t *testing.T) {
	presets := []protocol.PresetInfo{
		{Name: "work", Model: "anthropic/claude-sonnet-4-5", Labels: map[string]string{"context": "work"}},
		{Name: "cheap"},
	}
	var buf bytes.Buffer
	if err := renderPresets(&buf, presets, outputJSON, false); err != nil {
		t.Fatalf("renderPresets: %v", err)
	}

	var result map[string][]protocol.PresetInfo
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if len(result["presets"]) != 2 {
		t.Errorf("expected 2 presets, got %d", len(result["presets"]))
	}
	if result["presets"][0].Name != "work" {
		t.Errorf("first preset name = %q", result["presets"][0].Name)
	}
}

func TestRenderPresets_Table(t *testing.T) {
	presets := []protocol.PresetInfo{
		{Name: "work", Model: "anthropic/claude-sonnet-4-5", Labels: map[string]string{"context": "work"}},
	}
	var buf bytes.Buffer
	if err := renderPresets(&buf, presets, outputTable, false); err != nil {
		t.Fatalf("renderPresets: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "work") {
		t.Errorf("table does not contain preset name; output:\n%s", out)
	}
	if !strings.Contains(out, "anthropic/claude-sonnet-4-5") {
		t.Errorf("table does not contain model; output:\n%s", out)
	}
	for _, col := range []string{"NAME", "MODEL", "LABELS"} {
		if !strings.Contains(out, col) {
			t.Errorf("table missing column %q; output:\n%s", col, out)
		}
	}
}

// TestPresetsCommandAgreesWithLoadPresets pins Fix 3: `rafiki presets` and
// `rafiki create --preset` must read the same file. Before the fix,
// `rafiki presets` asked the DAEMON (ctrl_list_presets, which reads the
// daemon's own paths.PresetsFile() -- the old, unscoped location) while
// `loadPresets` (what `create --preset` uses) read the CLIENT's per-profile
// profile.PresetsFile(name). This seeds only the per-profile file and asserts
// the `presets` command's rendered output names the preset loadPresets sees.
func TestPresetsCommandAgreesWithLoadPresets(t *testing.T) {
	localProfileForTest(t, profile.Profile{})

	content := map[string]any{
		"presets": map[string]any{
			"fast": map[string]any{
				"model":  "anthropic/claude-haiku-4-5",
				"labels": map[string]string{"tier": "cheap"},
			},
		},
	}
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	path := profile.PresetsFile("test")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// What `rafiki create --preset fast` would resolve.
	pf, err := loadPresets("test")
	if err != nil {
		t.Fatalf("loadPresets: %v", err)
	}
	wantModel, ok := pf.Presets["fast"]
	if !ok {
		t.Fatalf("loadPresets did not see the seeded preset: %+v", pf.Presets)
	}

	// What `rafiki presets` renders. runPresets writes to os.Stdout directly
	// (like the daemon-backed version it replaces did), so capture the real
	// fd rather than cmd.SetOut.
	out := captureStdout(t, func() {
		cmd := newPresetsCmd()
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("rafiki presets: %v", err)
		}
	})
	if !strings.Contains(out, "fast") {
		t.Fatalf("`rafiki presets` output missing the preset `loadPresets` sees:\n%s", out)
	}
	if !strings.Contains(out, wantModel.Model) {
		t.Fatalf("`rafiki presets` output missing the model loadPresets resolved (%q):\n%s", wantModel.Model, out)
	}
}

// TestPresetsCommandOnABareProfileListsNothing checks that an absent presets
// file is zero rows, not an error -- matching the old ctrl_list_presets
// behavior ("An absent or empty presets file returns an empty slice").
func TestPresetsCommandOnABareProfileListsNothing(t *testing.T) {
	localProfileForTest(t, profile.Profile{})

	captureStdout(t, func() {
		cmd := newPresetsCmd()
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("rafiki presets with no presets file: %v", err)
		}
	})
}

// captureStdout redirects the real os.Stdout for the duration of fn and
// returns what was written to it. Needed for commands like runPresets that
// write to os.Stdout directly rather than through cmd.OutOrStdout().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wp
	fn()
	wp.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rp); err != nil {
		t.Fatal(err)
	}
	rp.Close()
	return buf.String()
}

func TestRenderPresets_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := renderPresets(&buf, nil, outputJSON, false); err != nil {
		t.Fatalf("renderPresets empty: %v", err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if string(result["presets"]) == "null" {
		t.Error("presets should be [] not null for empty slice")
	}
}

// ─── profile's `preset` field ────────────────────────────────────────────────
//
// RAFIKI_DEFAULT_PRESET is retired client-side (profile.CheckRetiredEnv
// rejects it via mustProfile); a profile's `preset` field replaces it.

// TestProfilePreset_AppliedWhenFlagUnset checks that the resolved profile's
// `preset` field is read (via resolvePresetName) when --preset is not passed.
func TestProfilePreset_AppliedWhenFlagUnset(t *testing.T) {
	localProfileForTest(t, profile.Profile{Preset: "mypreset"})

	content := map[string]any{
		"presets": map[string]any{
			"mypreset": map[string]any{
				"model":  "ollama/llama3.1:8b",
				"labels": map[string]string{"tier": "local"},
			},
		},
	}
	b, _ := json.Marshal(content)
	path := profile.PresetsFile("test")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate what runCreate does: read --preset flag (unset → ""), then
	// fall back to the profile's `preset` field.
	cmd := newCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("buildSpawnRequest: %v", err)
	}

	// Use the same resolution runCreate does, not a copy of it.
	presetName := resolvePresetName(cmd)
	if presetName != "mypreset" {
		t.Fatalf("presetName = %q, want mypreset", presetName)
	}

	pf, err := loadPresets("test")
	if err != nil {
		t.Fatalf("loadPresets: %v", err)
	}
	preset, ok := pf.Presets[presetName]
	if !ok {
		t.Fatalf("preset %q not found", presetName)
	}
	req.Model = resolveModel(req.Model, preset.Model, "", "")
	if len(preset.Labels) > 0 {
		req.Labels = mergeLabels(preset.Labels, req.Labels)
	}

	if req.Model != "ollama/llama3.1:8b" {
		t.Errorf("Model = %q, want ollama/llama3.1:8b", req.Model)
	}
	if req.Labels["tier"] != "local" {
		t.Errorf("Labels[tier] = %q, want local", req.Labels["tier"])
	}
}

func TestProfilePreset_FlagWinsOverProfile(t *testing.T) {
	// When --preset is set explicitly, the profile's `preset` field should be ignored.
	localProfileForTest(t, profile.Profile{Preset: "profpreset"})

	cmd := newCreateCmd()
	// Read the flag value — should be the default (empty), not the profile's.
	// The profile is only consulted in resolvePresetName, not in flag parsing.
	flagVal, _ := cmd.Flags().GetString("preset")
	if flagVal != "" {
		t.Errorf("unexpected flag default: %q", flagVal)
	}

	// Simulate setting --preset explicitly.
	if err := cmd.Flags().Set("preset", "flagpreset"); err != nil {
		t.Fatal(err)
	}
	// The flag wins and the profile default is skipped — asserted through the
	// real resolution rather than a copy of it.
	if presetName := resolvePresetName(cmd); presetName != "flagpreset" {
		t.Errorf("presetName = %q, want flagpreset (flag wins over profile default)", presetName)
	}
}
