package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// writePresetsFile writes a profile's presets.json and returns the path.
func writePresetsFile(t *testing.T, profileName string, content any) string {
	t.Helper()
	path := profile.PresetsFile(profileName)
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal presets: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write presets: %v", err)
	}
	return path
}

// setPresetsHome points HOME at dir and clears XDG_CONFIG_HOME so
// profile.PresetsFile(name) resolves deterministically under
// dir/.config/rafiki/profiles/<name>/, same as TestDefaultsFollowXDGSpec in
// internal/paths. The returned dir is rafiki's own config directory (still
// needed by callers checking the legacy-file report, which reads
// paths.PresetsFile() directly).
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

	_, err := loadPresets("test")
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

	_, err := loadPresets("test")
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
			pf, err := loadPresets("test")
			if err == nil {
				t.Fatalf("legacy presets file was read; got %+v, want an error", pf)
			}
			// But the error must point at it, and at the fix.
			if !containsAll(err.Error(), legacy, profile.PresetsFile("test"), "mv ") {
				t.Errorf("error %q should name the legacy file, the new file, and the mv to run", err.Error())
			}
		})
	}
}

// TestLoadPresets_MalformedJSON checks that bad JSON returns a helpful error.
func TestLoadPresets_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	setPresetsHome(t, dir)
	path := profile.PresetsFile("test")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadPresets("test")
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}
}

// TestLoadPresets_ValidFile checks normal load.
func TestLoadPresets_ValidFile(t *testing.T) {
	dir := t.TempDir()
	setPresetsHome(t, dir)
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
	writePresetsFile(t, "test", content)

	pf, err := loadPresets("test")
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

// TestPresetsAreScopedToTheProfile checks that two profiles' presets files
// never answer for each other.
func TestPresetsAreScopedToTheProfile(t *testing.T) {
	isolateProfiles(t)

	writeJSON := func(path string, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(profile.PresetsFile("work"), `{"presets":{"fast":{"model":"claude-opus-5"}}}`)
	writeJSON(profile.PresetsFile("personal"), `{"presets":{"fast":{"model":"openrouter/cheap"}}}`)

	w, err := loadPresets("work")
	if err != nil {
		t.Fatalf("loadPresets(work): %v", err)
	}
	if got := w.Presets["fast"].Model; got != "claude-opus-5" {
		t.Fatalf("work fast model = %q", got)
	}

	p, err := loadPresets("personal")
	if err != nil {
		t.Fatalf("loadPresets(personal): %v", err)
	}
	if got := p.Presets["fast"].Model; got != "openrouter/cheap" {
		t.Fatalf("personal fast model = %q — one profile's presets answered for the other", got)
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

// TestModelPrecedence pins the full chain. Read the plan's Task 10 before
// changing any row: the preset sitting ABOVE the profile default is a
// deliberate departure from the old RAFIKI_DEFAULT_MODEL ordering.
func TestModelPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		flagModel   string
		presetModel string
		profModel   string
		remembered  string
		want        string
	}{
		{"flag wins over everything", "flag-m", "preset-m", "prof-m", "remembered-m", "flag-m"},
		{"preset beats the profile default", "", "preset-m", "prof-m", "remembered-m", "preset-m"},
		{"profile default beats the remembered model", "", "", "prof-m", "remembered-m", "prof-m"},
		{"remembered is the last resort", "", "", "", "remembered-m", "remembered-m"},
		{"nothing set leaves it to the daemon", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveModel(tc.flagModel, tc.presetModel, tc.profModel, tc.remembered)
			if got != tc.want {
				t.Fatalf("resolveModel(%q,%q,%q,%q) = %q, want %q",
					tc.flagModel, tc.presetModel, tc.profModel, tc.remembered, got, tc.want)
			}
		})
	}
}

func TestKindDefaultsToTheProfilesKind(t *testing.T) {
	cases := []struct {
		name     string
		flagKind string
		profKind string
		want     string
	}{
		{"flag wins", protocol.KindClaude, protocol.KindFundi, protocol.KindClaude},
		{"profile supplies it", "", protocol.KindClaude, protocol.KindClaude},
		{"neither falls back to fundi", "", "", protocol.KindFundi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveKind(tc.flagKind, tc.profKind); got != tc.want {
				t.Fatalf("resolveKind(%q,%q) = %q, want %q", tc.flagKind, tc.profKind, got, tc.want)
			}
		})
	}
}

func TestProfileLabelsMergeUnderFlagLabels(t *testing.T) {
	prof := map[string]string{"env": "work", "team": "core"}
	flags := map[string]string{"env": "override"}
	got := mergeLabels(prof, flags)
	if got["env"] != "override" {
		t.Fatalf("env = %q, want the flag to win", got["env"])
	}
	if got["team"] != "core" {
		t.Fatalf("team = %q, want the profile's value to survive", got["team"])
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
