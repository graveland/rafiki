package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.graveland.dev/brent/fundi/internal/paths"
)

// Preset defines default model and label values applied by fundi create --preset NAME.
type Preset struct {
	Model  string            `json:"model,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// PresetsFile is the parsed form of the presets file.
type PresetsFile struct {
	Presets map[string]Preset `json:"presets"`
}

// legacyPresetsPaths are pre-move locations. They are probed only to turn "no
// presets file" into an error that says what to do; they are never read and
// never deleted. ~/.pi/agent held fundi's own presets file inside pi's
// directory; the pic- spelling predates the binary rename. Neither may equal
// paths.PresetsFile(), or the "legacy file still exists" branch would fire
// against the user's actual current file and report their own presets as
// stale — TestLoadPresets_LegacyFileIsNotReadButIsReported guards this.
func legacyPresetsPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".pi", "agent", "fundi-presets.json"),
		filepath.Join(home, ".pi", "agent", "pic-presets.json"),
	}
}

// loadPresets reads the presets file at paths.PresetsFile().
// Returns a specific error when the file is missing; otherwise wraps read/parse errors.
func loadPresets() (*PresetsFile, error) {
	path := paths.PresetsFile()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, missingPresetsError(path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pf PresetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &pf, nil
}

// missingPresetsError reports the absent presets file, naming any pre-move file
// still on disk. Failing with a bare "not found" while the user's presets sit
// at the old path would look like data loss.
func missingPresetsError(path string) error {
	for _, legacy := range legacyPresetsPaths() {
		if _, statErr := os.Stat(legacy); statErr == nil {
			return fmt.Errorf("no presets file at %s; %s still exists and is no longer read — move it:\n    mkdir -p %s && mv %s %s",
				path, legacy, filepath.Dir(path), legacy, path)
		}
	}
	return fmt.Errorf("no presets file at %s", path)
}

// availablePresets returns a sorted, comma-separated list of preset names.
// Returns "(none)" when the file has no presets.
func availablePresets(pf *PresetsFile) string {
	if pf == nil || len(pf.Presets) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(pf.Presets))
	for k := range pf.Presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
