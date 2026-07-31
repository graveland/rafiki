package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// TestListPresets_ReadsFromPathsPresetsFile locks down that the daemon reads
// presets from paths.PresetsFile() — the same single source of truth the CLI's
// loadPresets (cmd/fundi/presets.go) uses. If the two ever resolve different
// paths, `fundi presets` and the daemon's --preset resolution silently
// disagree: presets visibly listed by one side 404 on the other.
func TestListPresets_ReadsFromPathsPresetsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")

	configDir := filepath.Dir(paths.PresetsFile())
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"presets":{"daemon-side":{"model":"anthropic/claude-opus-4-7"}}}`
	if err := os.WriteFile(paths.PresetsFile(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Controller{}
	got, err := c.ListPresets(nil, nil)
	if err != nil {
		t.Fatalf("ListPresets: %v", err)
	}
	if len(got) != 1 || got[0].Name != "daemon-side" {
		t.Fatalf("ListPresets() = %+v, want one preset named daemon-side read from %s", got, paths.PresetsFile())
	}
}

// TestListPresets_MissingFileIsEmptyNotError checks the daemon's best-effort
// contract: an absent presets file at the new location yields an empty slice,
// not an error.
func TestListPresets_MissingFileIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")

	c := &Controller{}
	got, err := c.ListPresets(nil, nil)
	if err != nil {
		t.Fatalf("ListPresets: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListPresets() = %+v, want empty", got)
	}
}
