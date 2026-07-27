package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writePresetsFile writes a fundi-presets.json into dir and returns the path.
func writePresetsFile(t *testing.T, dir string, content any) string {
	t.Helper()
	path := filepath.Join(dir, "fundi-presets.json")
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal presets: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write presets: %v", err)
	}
	return path
}

// TestLoadPresets_MissingFile checks the specific error for a missing file.
func TestLoadPresets_MissingFile(t *testing.T) {
	// Point home to an empty temp dir so there's definitely no presets file.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected error for missing presets file")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
	// Error must mention the expected path.
	if !containsAll(err.Error(), "fundi-presets.json") {
		t.Errorf("error %q should mention fundi-presets.json", err.Error())
	}
}

// TestLoadPresets_LegacyFileIsNotReadButIsReported covers the rename from
// pic-presets.json: the old name is deliberately NOT a fallback, but failing
// with a bare "not found" while the user's presets sit on disk under the old
// spelling would read like data loss.
func TestLoadPresets_LegacyFileIsNotReadButIsReported(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(agentDir, legacyPresetsFileName)
	if err := os.WriteFile(legacy, []byte(`{"presets":{"old":{"model":"m"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	// The legacy file must not be loaded.
	pf, err := loadPresets()
	if err == nil {
		t.Fatalf("legacy presets file was read; got %+v, want an error", pf)
	}
	// But the error must point at it, and at the fix.
	if !containsAll(err.Error(), legacyPresetsFileName, PresetsFileName, "mv ") {
		t.Errorf("error %q should name the legacy file, the new file, and the mv to run", err.Error())
	}
}

// TestLoadPresets_MalformedJSON checks that bad JSON returns a helpful error.
func TestLoadPresets_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "fundi-presets.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}
}

// TestLoadPresets_ValidFile checks normal load.
func TestLoadPresets_ValidFile(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"presets": map[string]any{
			"cheap": map[string]any{
				"model":  "ollama/llama3.1:8b",
				"labels": map[string]string{"tier": "cheap"},
			},
			"smart": map[string]any{
				"model": "anthropic/claude-opus-4-7",
			},
		},
	}
	writePresetsFile(t, agentDir, content)
	t.Setenv("HOME", dir)

	pf, err := loadPresets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Presets["cheap"].Model != "ollama/llama3.1:8b" {
		t.Errorf("cheap.Model = %q", pf.Presets["cheap"].Model)
	}
	if pf.Presets["cheap"].Labels["tier"] != "cheap" {
		t.Errorf("cheap.Labels[tier] = %q", pf.Presets["cheap"].Labels["tier"])
	}
	if pf.Presets["smart"].Model != "anthropic/claude-opus-4-7" {
		t.Errorf("smart.Model = %q", pf.Presets["smart"].Model)
	}
}

// TestAvailablePresets checks the sorted name list.
func TestAvailablePresets(t *testing.T) {
	pf := &PresetsFile{
		Presets: map[string]Preset{
			"zebra": {Model: "a"},
			"alpha": {Model: "b"},
			"mango": {Model: "c"},
		},
	}
	got := availablePresets(pf)
	if got != "alpha, mango, zebra" {
		t.Errorf("got %q, want alpha, mango, zebra", got)
	}
}

// TestAvailablePresets_Empty returns "(none)".
func TestAvailablePresets_Empty(t *testing.T) {
	if got := availablePresets(nil); got != "(none)" {
		t.Errorf("got %q, want (none)", got)
	}
	if got := availablePresets(&PresetsFile{}); got != "(none)" {
		t.Errorf("got %q, want (none)", got)
	}
}

// TestPreset_MergeOrder checks that the merge precedence is
// preset < env-var defaults < explicit flags.
//
// We test this via buildSpawnRequest + manual preset merge (as runCreate does it).
func TestPreset_MergeOrder(t *testing.T) {
	// Preset: model=preset-model, labels: tier=smart, env=from-preset
	// PIC_DEFAULT_LABELS: env=from-env (overrides preset env)
	// --label: tier=override (overrides preset tier)
	t.Setenv("PIC_DEFAULT_LABELS", "env=from-env")
	os.Unsetenv("PIC_DEFAULT_MODEL")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("label", "tier=override"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("buildSpawnRequest: %v", err)
	}

	// Simulate preset application (as runCreate does).
	preset := Preset{
		Model:  "preset-model",
		Labels: map[string]string{"tier": "smart", "env": "from-preset"},
	}
	if req.Model == "" {
		req.Model = preset.Model
	}
	if len(preset.Labels) > 0 {
		req.Labels = mergeLabels(preset.Labels, req.Labels)
	}

	// Preset model should be used (nothing higher-priority set it).
	if req.Model != "preset-model" {
		t.Errorf("Model = %q, want preset-model", req.Model)
	}
	// PIC_DEFAULT_LABELS wins over preset for 'env'.
	if req.Labels["env"] != "from-env" {
		t.Errorf("Labels[env] = %q, want from-env (env-var wins over preset)", req.Labels["env"])
	}
	// --label wins over preset for 'tier'.
	if req.Labels["tier"] != "override" {
		t.Errorf("Labels[tier] = %q, want override (flag wins over preset)", req.Labels["tier"])
	}
}

// containsAll returns true when s contains all needles.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !containsStr(s, n) {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
