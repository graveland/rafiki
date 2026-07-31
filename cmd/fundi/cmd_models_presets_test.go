package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── renderModels ──────────────────────────────────────────────────────────────

func TestRenderModels_JSON(t *testing.T) {
	infos := []protocol.ModelInfo{
		{ID: "anthropic/claude-sonnet-4-5", Provider: "anthropic", Model: "claude-sonnet-4-5", Source: "builtin"},
		{ID: "anthropic-work/claude-sonnet-4-5", Provider: "anthropic-work", Model: "claude-sonnet-4-5", Name: "Work Sonnet", Source: "user-config"},
	}
	var buf bytes.Buffer
	if err := renderModels(&buf, infos, outputJSON, false); err != nil {
		t.Fatalf("renderModels: %v", err)
	}

	var result map[string][]protocol.ModelInfo
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if len(result["models"]) != 2 {
		t.Errorf("expected 2 models, got %d", len(result["models"]))
	}
	// Name should appear for user-config entry.
	found := false
	for _, m := range result["models"] {
		if m.Name == "Work Sonnet" {
			found = true
		}
	}
	if !found {
		t.Error("display name not present in JSON output")
	}
}

func TestRenderModels_Table(t *testing.T) {
	infos := []protocol.ModelInfo{
		{ID: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", Source: "builtin"},
	}
	var buf bytes.Buffer
	if err := renderModels(&buf, infos, outputTable, false); err != nil {
		t.Fatalf("renderModels: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "openai/gpt-4o") {
		t.Errorf("table does not contain model ID; output:\n%s", out)
	}
	if !strings.Contains(out, "builtin") {
		t.Errorf("table does not contain source; output:\n%s", out)
	}
	// Column headers.
	for _, col := range []string{"ID", "PROVIDER", "MODEL", "SOURCE"} {
		if !strings.Contains(out, col) {
			t.Errorf("table missing column %q; output:\n%s", col, out)
		}
	}
}

func TestRenderModels_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModels(&buf, nil, outputJSON, false); err != nil {
		t.Fatalf("renderModels empty: %v", err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// "models" must be [] not null.
	if string(result["models"]) == "null" {
		t.Error("models should be [] not null for empty slice")
	}
}

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

// ─── FUNDI_DEFAULT_PRESET ───────────────────────────────────────────────────────

// TestFundiDefaultPreset_AppliedWhenFlagUnset checks that FUNDI_DEFAULT_PRESET
// is read in runCreate when --preset is not passed.  We test this by calling
// the preset-resolution block inline (as runCreate does) rather than spinning
// up a real daemon.
func TestFundiDefaultPreset_AppliedWhenFlagUnset(t *testing.T) {
	// Write a minimal presets file into a temp home dir's fundi config dir.
	dir := t.TempDir()
	configDir := setPresetsHome(t, dir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"presets": map[string]any{
			"mypreset": map[string]any{
				"model":  "ollama/llama3.1:8b",
				"labels": map[string]string{"tier": "local"},
			},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(configDir, "presets.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUNDI_DEFAULT_PRESET", "mypreset")
	os.Unsetenv("FUNDI_DEFAULT_MODEL")
	os.Unsetenv("FUNDI_DEFAULT_LABELS")

	// Simulate what runCreate does: read --preset flag (unset → ""), then
	// fall back to FUNDI_DEFAULT_PRESET.
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

	pf, err := loadPresets()
	if err != nil {
		t.Fatalf("loadPresets: %v", err)
	}
	preset, ok := pf.Presets[presetName]
	if !ok {
		t.Fatalf("preset %q not found", presetName)
	}
	if req.Model == "" && preset.Model != "" {
		req.Model = preset.Model
	}
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

func TestFundiDefaultPreset_FlagWinsOverEnvVar(t *testing.T) {
	// When --preset is set explicitly, FUNDI_DEFAULT_PRESET should be ignored.
	t.Setenv("FUNDI_DEFAULT_PRESET", "envpreset")

	cmd := newCreateCmd()
	// Read the flag value — should be the default (empty), not the env var.
	// The env var is only consulted in runCreate, not in flag parsing.
	flagVal, _ := cmd.Flags().GetString("preset")
	if flagVal != "" {
		t.Errorf("unexpected flag default: %q", flagVal)
	}

	// Simulate setting --preset explicitly.
	if err := cmd.Flags().Set("preset", "flagpreset"); err != nil {
		t.Fatal(err)
	}
	// The flag wins and the env var is skipped — asserted through the real
	// resolution rather than a copy of it.
	if presetName := resolvePresetName(cmd); presetName != "flagpreset" {
		t.Errorf("presetName = %q, want flagpreset (flag wins over env var)", presetName)
	}
}
