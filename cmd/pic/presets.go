package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Preset defines default model and label values applied by pic create --preset NAME.
type Preset struct {
	Model  string            `json:"model,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// PresetsFile is the parsed form of the presets file.
type PresetsFile struct {
	Presets map[string]Preset `json:"presets"`
}

// PresetsFileName is the presets file's name inside pi's agent directory. The
// file is fundi's, so it carries fundi's name; the directory is pi's, which is
// why it is not resolved through internal/paths.
const PresetsFileName = "fundi-presets.json"

// legacyPresetsFileName is the pre-rename spelling. It is deliberately NOT read
// as a fallback — it is only probed to turn "no presets file" into an error that
// says what to do about it.
const legacyPresetsFileName = "pic-presets.json"

// presetsPath returns the presets file location: ~/.pi/agent/<name>.
func presetsPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", name), nil
}

// loadPresets reads ~/.pi/agent/fundi-presets.json.
// Returns a specific error when the file is missing; otherwise wraps read/parse errors.
func loadPresets() (*PresetsFile, error) {
	path, err := presetsPath(PresetsFileName)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, missingPresetsError(path)
		}
		return nil, fmt.Errorf("read %s: %w", PresetsFileName, err)
	}
	var pf PresetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", PresetsFileName, err)
	}
	return &pf, nil
}

// missingPresetsError reports the absent presets file, and says so explicitly
// when the pre-rename file is sitting right next to it. The old name is not
// read — but failing with a bare "not found" while the user's presets are on
// disk under the previous spelling would look like data loss.
func missingPresetsError(path string) error {
	if legacy, err := presetsPath(legacyPresetsFileName); err == nil {
		if _, statErr := os.Stat(legacy); statErr == nil {
			return fmt.Errorf("no presets file at %s; %s still exists and is no longer read — rename it:\n    mv %s %s",
				path, legacyPresetsFileName, legacy, path)
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
