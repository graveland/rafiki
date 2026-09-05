package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Preset defines default model and label values applied by rafiki create --preset NAME.
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
// never deleted. ~/.pi/agent held rafiki's own presets file inside pi's
// directory; the pic- spelling predates the binary rename. Neither may equal
// profile.PresetsFile(name), or the "legacy file still exists" branch would fire
// against the user's actual current file and report their own presets as
// stale — TestLoadPresets_LegacyFileIsNotReadButIsReported guards this.
func legacyPresetsPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".pi", "agent", "rafiki-presets.json"),
		filepath.Join(home, ".pi", "agent", "pic-presets.json"),
	}
}

// loadPresets reads the presets file at profile.PresetsFile(profileName).
// Returns a specific error when the file is missing; otherwise wraps read/parse errors.
func loadPresets(profileName string) (*PresetsFile, error) {
	path := profile.PresetsFile(profileName)
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

// presetsForListing reads a profile's presets file for `rafiki presets`,
// treating an absent file as zero presets rather than an error -- matching
// the behavior ctrl_list_presets always had ("An absent or empty presets
// file returns an empty slice (not an error)"). loadPresets itself keeps
// erroring on a missing file, because there the caller named a specific
// --preset and the file genuinely ought to exist.
func presetsForListing(profileName string) (*PresetsFile, error) {
	pf, err := loadPresets(profileName)
	if err != nil {
		path := profile.PresetsFile(profileName)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return &PresetsFile{}, nil
		}
		return nil, err
	}
	return pf, nil
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

// matchesPresetLabelFilter returns true when a preset's labels satisfy both
// the AND-match required map and the key-presence hasLabels list. Mirrors
// cmd/rafikid/models_presets.go's matchesPresetLabelFilter -- presets are a
// client-side concept now (see loadPresets), so this package keeps its own
// copy rather than importing the daemon.
func matchesPresetLabelFilter(presetLabels, required map[string]string, hasLabels []string) bool {
	for k, v := range required {
		if presetLabels[k] != v {
			return false
		}
	}
	for _, k := range hasLabels {
		if _, ok := presetLabels[k]; !ok {
			return false
		}
	}
	return true
}

// presetInfos converts a parsed presets file into sorted, filtered
// protocol.PresetInfo rows -- the same shape `rafiki presets` has always
// rendered, now built from the client's own per-profile file instead of the
// daemon's ctrl_list_presets response.
func presetInfos(pf *PresetsFile, labels map[string]string, hasLabel []string) []protocol.PresetInfo {
	if pf == nil {
		return nil
	}
	names := make([]string, 0, len(pf.Presets))
	for name := range pf.Presets {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]protocol.PresetInfo, 0, len(names))
	for _, name := range names {
		p := pf.Presets[name]
		if !matchesPresetLabelFilter(p.Labels, labels, hasLabel) {
			continue
		}
		out = append(out, protocol.PresetInfo{Name: name, Model: p.Model, Labels: p.Labels})
	}
	return out
}
