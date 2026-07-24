package analyze

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadProfiles loads a YAML file containing profiles and returns them as a map.
// The YAML structure is: name -> profile settings.
// Each profile's Name field is set from its map key, and Defaults() is called on each.
// Unknown fields in the YAML (including nested fields) will cause an error.
func LoadProfiles(path string) (map[string]Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	profilesRaw := make(map[string]Profile)
	err = dec.Decode(&profilesRaw)
	if err != nil {
		return nil, err
	}

	profiles := make(map[string]Profile)
	for name, p := range profilesRaw {
		p.Name = name
		p.Defaults()
		profiles[name] = p
	}

	return profiles, nil
}

// Defaults applies default values to a Profile if they are not already set.
//   - Limit defaults to 50 if 0
//   - CompactPolicy fields default to: MaxToolResultBytes=2048, MaxTranscriptBytes=300*1024,
//     KeepFirstTurns=4, KeepLastTurns=20
func (p *Profile) Defaults() {
	if p.Limit == 0 {
		p.Limit = 50
	}

	if p.Compact.MaxToolResultBytes == 0 {
		p.Compact.MaxToolResultBytes = 2048
	}
	if p.Compact.MaxTranscriptBytes == 0 {
		p.Compact.MaxTranscriptBytes = 300 * 1024
	}
	if p.Compact.KeepFirstTurns == 0 {
		p.Compact.KeepFirstTurns = 4
	}
	if p.Compact.KeepLastTurns == 0 {
		p.Compact.KeepLastTurns = 20
	}
}

// EffectiveDetectorPrompt resolves the effective detector prompt.
// If DetectorPrompt is non-empty, it fully replaces the builtin.
// If DetectorPrompt is empty and DetectorPromptExtra is non-empty, the builtin is used with the extra appended after a blank line.
// If both DetectorPrompt and DetectorPromptExtra are non-empty, DetectorPrompt replaces the builtin and DetectorPromptExtra is appended after a blank line.
// If both are empty, the builtin is used as-is.
func (p *Profile) EffectiveDetectorPrompt(builtin string) string {
	if p.DetectorPrompt != "" {
		// Replace mode: DetectorPrompt fully replaces builtin
		base := p.DetectorPrompt
		if p.DetectorPromptExtra != "" {
			return base + "\n\n" + p.DetectorPromptExtra
		}
		return base
	}

	// DetectorPrompt is empty, use builtin
	if p.DetectorPromptExtra != "" {
		return builtin + "\n\n" + p.DetectorPromptExtra
	}

	return builtin
}

// EffectiveDraftPrompt resolves the effective draft prompt.
// If DraftPrompt is non-empty, it fully replaces the builtin.
// If DraftPrompt is empty and DraftPromptExtra is non-empty, the builtin is used with the extra appended after a blank line.
// If both DraftPrompt and DraftPromptExtra are non-empty, DraftPrompt replaces the builtin and DraftPromptExtra is appended after a blank line.
// If both are empty, the builtin is used as-is.
func (p *Profile) EffectiveDraftPrompt(builtin string) string {
	if p.DraftPrompt != "" {
		// Replace mode: DraftPrompt fully replaces builtin
		base := p.DraftPrompt
		if p.DraftPromptExtra != "" {
			return base + "\n\n" + p.DraftPromptExtra
		}
		return base
	}

	// DraftPrompt is empty, use builtin
	if p.DraftPromptExtra != "" {
		return builtin + "\n\n" + p.DraftPromptExtra
	}

	return builtin
}

// PromptHash returns a sha256 hex hash of the four prompt configuration fields
// (DetectorPrompt, DetectorPromptExtra, DraftPrompt, DraftPromptExtra).
// Returns "" if all four fields are empty.
// The hash is stable across calls with the same field values.
// The hash identifies the prompt configuration, not including builtin text.
func (p *Profile) PromptHash() string {
	if p.DetectorPrompt == "" && p.DetectorPromptExtra == "" &&
		p.DraftPrompt == "" && p.DraftPromptExtra == "" {
		return ""
	}

	// Join the four fields with a separator (\x00)
	combined := p.DetectorPrompt + "\x00" + p.DetectorPromptExtra + "\x00" +
		p.DraftPrompt + "\x00" + p.DraftPromptExtra

	hash := sha256.Sum256([]byte(combined))
	return fmt.Sprintf("%x", hash)
}
