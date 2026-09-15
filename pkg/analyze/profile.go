// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"gopkg.in/yaml.v3"
)

// decodeProfilesYAML strictly decodes a profiles.yaml document (name ->
// profile settings), setting each Profile's Name from its map key and
// calling Defaults(). Unknown fields (including nested fields) are an error.
func decodeProfilesYAML(data []byte) (map[string]Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	profilesRaw := make(map[string]Profile)
	if err := dec.Decode(&profilesRaw); err != nil {
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

// LoadProfiles loads a YAML file containing profiles and returns them as a map.
// The YAML structure is: name -> profile settings.
// Each profile's Name field is set from its map key, and Defaults() is called on each.
// Unknown fields in the YAML (including nested fields) will cause an error.
//
// The *_file prompt-reference fields (detector_prompt_file,
// detector_prompt_extra_file, draft_prompt_file, draft_prompt_extra_file)
// are only supported in analyzer directories (LoadAnalyzerDir); if set here,
// LoadProfiles returns an error.
func LoadProfiles(path string) (map[string]Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	profiles, err := decodeProfilesYAML(data)
	if err != nil {
		return nil, err
	}

	for name, p := range profiles {
		if p.DetectorPromptFile != "" || p.DetectorPromptExtraFile != "" ||
			p.DraftPromptFile != "" || p.DraftPromptExtraFile != "" {
			return nil, fmt.Errorf("profile %q: *_prompt_file fields are only supported in analyzer directories (LoadAnalyzerDir), not a single profiles file", name)
		}
	}

	return profiles, nil
}

// AnalyzerConfig is the result of loading an analyzer directory: the
// profiles it defines, plus the two BASE prompts (detector.md/draft.md)
// shared by every profile in the directory.
type AnalyzerConfig struct {
	Profiles     map[string]Profile
	DetectorBase string
	DraftBase    string
}

// LoadAnalyzerDir loads an analyzer directory from disk: dir/profiles.yaml
// (required, same strict schema as LoadProfiles) plus dir/detector.md and
// dir/draft.md (optional BASE prompts for the two LLM stages; "" if absent).
// A thin wrapper over LoadAnalyzerDirFS(os.DirFS(dir)) — see that function
// for the full behavior (prompt-file resolution, base-prompt precedence).
func LoadAnalyzerDir(dir string) (*AnalyzerConfig, error) {
	return LoadAnalyzerDirFS(os.DirFS(dir))
}

// LoadAnalyzerDirFS loads an analyzer directory from fsys: profiles.yaml
// (required, same strict schema as LoadProfiles) plus detector.md and
// draft.md (optional BASE prompts for the two LLM stages; "" if absent).
// fsys works equally over a real directory (os.DirFS) or an embedded one
// (embed.FS, optionally narrowed with fs.Sub) -- io/fs paths are always
// forward-slash, which is why this function and its helpers use "path", not
// "path/filepath".
//
// Per profile, the *_file fields (detector_prompt_file,
// detector_prompt_extra_file, draft_prompt_file, draft_prompt_extra_file)
// name a file, RELATIVE to fsys's root, whose contents are read into the
// corresponding inline field (detector_prompt, detector_prompt_extra,
// draft_prompt, draft_prompt_extra); the _file field is then zeroed. Paths
// must not be absolute, must not use "\", and must not contain any ".."
// segment (same policy as draft.go's validateRelativePath). Setting both an
// inline field and its _file twin is an error ("choose one").
//
// LoadAnalyzerDirFS does NOT set DetectorPromptBase/DraftPromptBase on the
// returned profiles — those live on AnalyzerConfig, not on each Profile.
// Attaching the base to a profile is a resolution-layer concern: each
// consumer (CLI inline profile, corpus run, server) may layer in a local
// override of the analyzer dir's base before resolving prompts, so the
// loader hands back the raw base text and lets the caller decide.
func LoadAnalyzerDirFS(fsys fs.FS) (*AnalyzerConfig, error) {
	data, err := fs.ReadFile(fsys, "profiles.yaml")
	if err != nil {
		return nil, fmt.Errorf("analyze: load analyzer dir: %w", err)
	}

	profiles, err := decodeProfilesYAML(data)
	if err != nil {
		return nil, fmt.Errorf("analyze: load analyzer dir: profiles.yaml: %w", err)
	}

	for name, p := range profiles {
		p, err := resolvePromptFiles(fsys, name, p)
		if err != nil {
			return nil, fmt.Errorf("analyze: load analyzer dir: %w", err)
		}
		profiles[name] = p
	}

	detectorBase, err := readOptionalFile(fsys, "detector.md")
	if err != nil {
		return nil, fmt.Errorf("analyze: load analyzer dir: %w", err)
	}
	draftBase, err := readOptionalFile(fsys, "draft.md")
	if err != nil {
		return nil, fmt.Errorf("analyze: load analyzer dir: %w", err)
	}

	return &AnalyzerConfig{
		Profiles:     profiles,
		DetectorBase: detectorBase,
		DraftBase:    draftBase,
	}, nil
}

// resolvePromptFiles resolves a profile's *_file fields against fsys, one at
// a time, returning the profile with each inline field populated and its
// _file twin cleared.
func resolvePromptFiles(fsys fs.FS, name string, p Profile) (Profile, error) {
	resolve := func(inline, file, fieldName string) (string, error) {
		if file == "" {
			return inline, nil
		}
		if inline != "" {
			return "", fmt.Errorf("profile %q: %s and %s_file both set; choose one", name, fieldName, fieldName)
		}
		if err := validateRelativePath(file); err != nil {
			return "", fmt.Errorf("profile %q: %s_file %q: %w", name, fieldName, file, err)
		}
		content, err := fs.ReadFile(fsys, path.Clean(file))
		if err != nil {
			return "", fmt.Errorf("profile %q: %s_file %q: %w", name, fieldName, file, err)
		}
		return string(content), nil
	}

	var err error
	if p.DetectorPrompt, err = resolve(p.DetectorPrompt, p.DetectorPromptFile, "detector_prompt"); err != nil {
		return Profile{}, err
	}
	p.DetectorPromptFile = ""
	if p.DetectorPromptExtra, err = resolve(p.DetectorPromptExtra, p.DetectorPromptExtraFile, "detector_prompt_extra"); err != nil {
		return Profile{}, err
	}
	p.DetectorPromptExtraFile = ""
	if p.DraftPrompt, err = resolve(p.DraftPrompt, p.DraftPromptFile, "draft_prompt"); err != nil {
		return Profile{}, err
	}
	p.DraftPromptFile = ""
	if p.DraftPromptExtra, err = resolve(p.DraftPromptExtra, p.DraftPromptExtraFile, "draft_prompt_extra"); err != nil {
		return Profile{}, err
	}
	p.DraftPromptExtraFile = ""

	return p, nil
}

// readOptionalFile reads name from fsys, returning "" (no error) if it
// doesn't exist.
func readOptionalFile(fsys fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// Defaults applies default values to a Profile if they are not already set.
//   - Limit defaults to 50 if 0
//   - MaxOutputTokens defaults to 16384 if 0
//   - CompactPolicy fields default to: MaxToolResultBytes=2048, MaxTranscriptBytes=300*1024,
//     KeepFirstTurns=4, KeepLastTurns=20
func (p *Profile) Defaults() {
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.MaxOutputTokens == 0 {
		p.MaxOutputTokens = 16384
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

// EffectiveDetectorPrompt resolves the effective detector prompt. Base
// precedence: DetectorPrompt (profile replacement) > DetectorPromptBase
// (analyzer-dir base) > builtin (the Go fallback, used only when neither of
// the above is set). DetectorPromptExtra, if non-empty, is then appended
// after a blank line to whichever base was selected.
func (p *Profile) EffectiveDetectorPrompt(builtin string) string {
	base := builtin
	if p.DetectorPromptBase != "" {
		base = p.DetectorPromptBase
	}
	if p.DetectorPrompt != "" {
		base = p.DetectorPrompt
	}

	if p.DetectorPromptExtra != "" {
		return base + "\n\n" + p.DetectorPromptExtra
	}
	return base
}

// EffectiveDraftPrompt resolves the effective draft prompt. Base precedence:
// DraftPrompt (profile replacement) > DraftPromptBase (analyzer-dir base) >
// builtin (the Go fallback, used only when neither of the above is set).
// DraftPromptExtra, if non-empty, is then appended after a blank line to
// whichever base was selected.
func (p *Profile) EffectiveDraftPrompt(builtin string) string {
	base := builtin
	if p.DraftPromptBase != "" {
		base = p.DraftPromptBase
	}
	if p.DraftPrompt != "" {
		base = p.DraftPrompt
	}

	if p.DraftPromptExtra != "" {
		return base + "\n\n" + p.DraftPromptExtra
	}
	return base
}

// PromptHash returns a sha256 hex hash of all six prompt-bearing fields
// (DetectorPromptBase, DetectorPrompt, DetectorPromptExtra, DraftPromptBase,
// DraftPrompt, DraftPromptExtra). Returns "" if all six fields are empty.
// The hash is stable across calls with the same field values.
//
// Bases are included deliberately: an analyzer-dir edit to detector.md or
// draft.md changes what the model actually sees just as much as a profile
// override does, and must invalidate the analysis skip-key (the
// already-analyzed check keyed on prompt_hash) the same way.
func (p *Profile) PromptHash() string {
	if p.DetectorPromptBase == "" && p.DetectorPrompt == "" && p.DetectorPromptExtra == "" &&
		p.DraftPromptBase == "" && p.DraftPrompt == "" && p.DraftPromptExtra == "" {
		return ""
	}

	// Join the six fields with a separator (\x00)
	combined := p.DetectorPromptBase + "\x00" + p.DetectorPrompt + "\x00" + p.DetectorPromptExtra + "\x00" +
		p.DraftPromptBase + "\x00" + p.DraftPrompt + "\x00" + p.DraftPromptExtra

	hash := sha256.Sum256([]byte(combined))
	return fmt.Sprintf("%x", hash)
}
