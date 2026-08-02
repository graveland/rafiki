package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// writePresetsFile writes a presets.json into dir and returns the path.
func writePresetsFile(t *testing.T, dir string, content any) string {
	t.Helper()
	path := filepath.Join(dir, "presets.json")
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal presets: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write presets: %v", err)
	}
	return path
}

// setPresetsHome points HOME at dir and clears XDG_CONFIG_HOME so
// paths.PresetsFile() resolves deterministically to dir/.config/rafiki/presets.json,
// same as TestDefaultsFollowXDGSpec in internal/paths.
func setPresetsHome(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	return filepath.Join(dir, ".config", "rafiki")
}

// TestLoadPresets_MissingFile checks the specific error for a missing file.
func TestLoadPresets_MissingFile(t *testing.T) {
	// Point home to an empty temp dir so there's definitely no presets file.
	dir := t.TempDir()
	setPresetsHome(t, dir)

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected error for missing presets file")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
	// Error must mention the expected path.
	if !containsAll(err.Error(), "presets.json") {
		t.Errorf("error %q should mention presets.json", err.Error())
	}
}

// TestPresetsPath_IsUnderRafikiConfigDir locks down that rafiki's presets file
// lives under rafiki's own config directory, not inside pi's.
func TestPresetsPath_IsUnderRafikiConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	got := paths.PresetsFile()
	if got != "/tmp/cfg/rafiki/presets.json" {
		t.Fatalf("presets path = %q, want /tmp/cfg/rafiki/presets.json", got)
	}
	if strings.Contains(got, "/.pi/") {
		t.Error("presets must not live in pi's directory")
	}
}

// TestMissingPresets_ReportsLegacyPiLocation covers the move out of pi's
// directory: the legacy file (rafiki's own presets, formerly inside ~/.pi/agent)
// is deliberately NOT read as a fallback, but failing with a bare "not found"
// while the user's presets sit on disk under the old path would look like data
// loss.
func TestMissingPresets_ReportsLegacyPiLocation(t *testing.T) {
	home := t.TempDir()
	setPresetsHome(t, home)

	legacyDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "rafiki-presets.json")
	if err := os.WriteFile(legacy, []byte(`{"presets":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected an error for a missing presets file")
	}
	if !strings.Contains(err.Error(), legacy) {
		t.Errorf("error must name the legacy file so it does not look like data loss; got: %v", err)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Error("the legacy file must never be deleted")
	}
}

// TestLoadPresets_LegacyFileIsNotReadButIsReported covers both pre-move
// locations: ~/.pi/agent/rafiki-presets.json (rafiki's own file, inside pi's
// directory) and ~/.pi/agent/pic-presets.json (the pre-rename spelling).
// Neither is a fallback — they are only probed to turn "no presets file" into
// an error that says what to do about it.
func TestLoadPresets_LegacyFileIsNotReadButIsReported(t *testing.T) {
	for _, legacyName := range []string{"rafiki-presets.json", "pic-presets.json"} {
		t.Run(legacyName, func(t *testing.T) {
			dir := t.TempDir()
			agentDir := filepath.Join(dir, ".pi", "agent")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(agentDir, legacyName)
			if err := os.WriteFile(legacy, []byte(`{"presets":{"old":{"model":"m"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			setPresetsHome(t, dir)

			// The legacy file must not be loaded.
			pf, err := loadPresets()
			if err == nil {
				t.Fatalf("legacy presets file was read; got %+v, want an error", pf)
			}
			// But the error must point at it, and at the fix.
			if !containsAll(err.Error(), legacy, paths.PresetsFile(), "mv ") {
				t.Errorf("error %q should name the legacy file, the new file, and the mv to run", err.Error())
			}
		})
	}
}

// TestLoadPresets_MalformedJSON checks that bad JSON returns a helpful error.
func TestLoadPresets_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	configDir := setPresetsHome(t, dir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "presets.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}
}

// TestLoadPresets_ValidFile checks normal load.
func TestLoadPresets_ValidFile(t *testing.T) {
	dir := t.TempDir()
	configDir := setPresetsHome(t, dir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
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
	writePresetsFile(t, configDir, content)

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
	// RAFIKI_DEFAULT_LABELS: env=from-env (overrides preset env)
	// --label: tier=override (overrides preset tier)
	t.Setenv("RAFIKI_DEFAULT_LABELS", "env=from-env")
	os.Unsetenv("RAFIKI_DEFAULT_MODEL")

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
	// RAFIKI_DEFAULT_LABELS wins over preset for 'env'.
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
