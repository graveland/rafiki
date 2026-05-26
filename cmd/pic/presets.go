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

// PresetsFile is the parsed form of ~/.pi/agent/pic-presets.json.
type PresetsFile struct {
	Presets map[string]Preset `json:"presets"`
}

// loadPresets reads ~/.pi/agent/pic-presets.json.
// Returns a specific error when the file is missing; otherwise wraps read/parse errors.
func loadPresets() (*PresetsFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	path := filepath.Join(home, ".pi", "agent", "pic-presets.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no presets file at ~/.pi/agent/pic-presets.json")
		}
		return nil, fmt.Errorf("read pic-presets.json: %w", err)
	}
	var pf PresetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("parse pic-presets.json: %w", err)
	}
	return &pf, nil
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
