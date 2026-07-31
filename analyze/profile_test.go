// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfiles(t *testing.T) {
	// Create a temporary YAML file with two profiles
	yamlContent := `
basic:
  detector_model: claude-opus-4-20250805
  rank_model: claude-haiku-4-5-20251001
  draft_model: claude-opus-4-20250805
  limit: 100

minimal:
  detector_model: gpt-4
`

	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "profiles.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	profiles, err := LoadProfiles(yamlPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	// Check basic profile
	basic, ok := profiles["basic"]
	if !ok {
		t.Fatal("expected 'basic' profile")
	}
	if basic.Name != "basic" {
		t.Errorf("expected Name='basic', got %q", basic.Name)
	}
	if basic.DetectorModel != "claude-opus-4-20250805" {
		t.Errorf("expected DetectorModel='claude-opus-4-20250805', got %q", basic.DetectorModel)
	}
	if basic.RankModel != "claude-haiku-4-5-20251001" {
		t.Errorf("expected RankModel='claude-haiku-4-5-20251001', got %q", basic.RankModel)
	}
	if basic.DraftModel != "claude-opus-4-20250805" {
		t.Errorf("expected DraftModel='claude-opus-4-20250805', got %q", basic.DraftModel)
	}
	if basic.Limit != 100 {
		t.Errorf("expected Limit=100, got %d", basic.Limit)
	}

	// Check minimal profile
	minimal, ok := profiles["minimal"]
	if !ok {
		t.Fatal("expected 'minimal' profile")
	}
	if minimal.Name != "minimal" {
		t.Errorf("expected Name='minimal', got %q", minimal.Name)
	}
	if minimal.DetectorModel != "gpt-4" {
		t.Errorf("expected DetectorModel='gpt-4', got %q", minimal.DetectorModel)
	}
}

func TestDefaults(t *testing.T) {
	p := &Profile{
		DetectorModel: "claude-opus-4-20250805",
		DraftModel:    "claude-opus-4-20250805",
	}
	p.Defaults()

	if p.Limit != 50 {
		t.Errorf("expected Limit=50, got %d", p.Limit)
	}
	if p.MaxOutputTokens != 16384 {
		t.Errorf("expected MaxOutputTokens=16384, got %d", p.MaxOutputTokens)
	}
	if p.Compact.MaxToolResultBytes != 2048 {
		t.Errorf("expected MaxToolResultBytes=2048, got %d", p.Compact.MaxToolResultBytes)
	}
	if p.Compact.MaxTranscriptBytes != 300*1024 {
		t.Errorf("expected MaxTranscriptBytes=307200, got %d", p.Compact.MaxTranscriptBytes)
	}
	if p.Compact.KeepFirstTurns != 4 {
		t.Errorf("expected KeepFirstTurns=4, got %d", p.Compact.KeepFirstTurns)
	}
	if p.Compact.KeepLastTurns != 20 {
		t.Errorf("expected KeepLastTurns=20, got %d", p.Compact.KeepLastTurns)
	}
}

func TestDefaultsPreservesExisting(t *testing.T) {
	p := &Profile{
		DetectorModel:   "claude-opus-4-20250805",
		DraftModel:      "claude-opus-4-20250805",
		Limit:           75,
		MaxOutputTokens: 8192,
		Compact: CompactPolicy{
			MaxToolResultBytes: 4096,
			MaxTranscriptBytes: 500 * 1024,
			KeepFirstTurns:     2,
			KeepLastTurns:      30,
		},
	}
	p.Defaults()

	if p.Limit != 75 {
		t.Errorf("expected Limit=75 (preserved), got %d", p.Limit)
	}
	if p.MaxOutputTokens != 8192 {
		t.Errorf("expected MaxOutputTokens=8192 (preserved), got %d", p.MaxOutputTokens)
	}
	if p.Compact.MaxToolResultBytes != 4096 {
		t.Errorf("expected MaxToolResultBytes=4096 (preserved), got %d", p.Compact.MaxToolResultBytes)
	}
	if p.Compact.MaxTranscriptBytes != 500*1024 {
		t.Errorf("expected MaxTranscriptBytes=512000 (preserved), got %d", p.Compact.MaxTranscriptBytes)
	}
	if p.Compact.KeepFirstTurns != 2 {
		t.Errorf("expected KeepFirstTurns=2 (preserved), got %d", p.Compact.KeepFirstTurns)
	}
	if p.Compact.KeepLastTurns != 30 {
		t.Errorf("expected KeepLastTurns=30 (preserved), got %d", p.Compact.KeepLastTurns)
	}
}

func TestUnknownFieldError(t *testing.T) {
	yamlContent := `
test:
  detector_model: claude-opus-4-20250805
  draft_model: claude-opus-4-20250805
  unknown_field: invalid
`

	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "profiles.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadProfiles(yamlPath)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestCompactFieldsFromYAML(t *testing.T) {
	yamlContent := `
custom:
  detector_model: claude-opus-4-20250805
  draft_model: claude-opus-4-20250805
  compact:
    max_tool_result_bytes: 999
    max_transcript_bytes: 500000
    keep_first_turns: 2
    keep_last_turns: 10
`

	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "profiles.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	profiles, err := LoadProfiles(yamlPath)
	if err != nil {
		t.Fatal(err)
	}

	custom := profiles["custom"]
	if custom.Compact.MaxToolResultBytes != 999 {
		t.Errorf("expected MaxToolResultBytes=999, got %d", custom.Compact.MaxToolResultBytes)
	}
	if custom.Compact.MaxTranscriptBytes != 500000 {
		t.Errorf("expected MaxTranscriptBytes=500000, got %d", custom.Compact.MaxTranscriptBytes)
	}
	if custom.Compact.KeepFirstTurns != 2 {
		t.Errorf("expected KeepFirstTurns=2, got %d", custom.Compact.KeepFirstTurns)
	}
	if custom.Compact.KeepLastTurns != 10 {
		t.Errorf("expected KeepLastTurns=10, got %d", custom.Compact.KeepLastTurns)
	}
}

func TestUnknownNestedFieldError(t *testing.T) {
	yamlContent := `
test:
  detector_model: claude-opus-4-20250805
  draft_model: claude-opus-4-20250805
  compact:
    max_tool_result_bytes: 999
    unknown_nested_field: invalid
`

	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "profiles.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadProfiles(yamlPath)
	if err == nil {
		t.Fatal("expected error for unknown nested field")
	}
}

func TestLoadAnalyzerDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
default:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
`)
	writeFile(t, dir, "detector.md", "detector base text")
	writeFile(t, dir, "draft.md", "draft base text")

	cfg, err := LoadAnalyzerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DetectorBase != "detector base text" {
		t.Errorf("expected DetectorBase=%q, got %q", "detector base text", cfg.DetectorBase)
	}
	if cfg.DraftBase != "draft base text" {
		t.Errorf("expected DraftBase=%q, got %q", "draft base text", cfg.DraftBase)
	}
	p, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatal("expected 'default' profile")
	}
	if p.Name != "default" {
		t.Errorf("expected Name='default', got %q", p.Name)
	}
	// LoadAnalyzerDir must not itself attach the bases to profiles — that's
	// a resolution-layer concern.
	if p.DetectorPromptBase != "" || p.DraftPromptBase != "" {
		t.Errorf("expected LoadAnalyzerDir to leave *PromptBase unset on profiles, got %q / %q",
			p.DetectorPromptBase, p.DraftPromptBase)
	}
}

func TestLoadAnalyzerDirMissingMdFilesOK(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
default:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
`)

	cfg, err := LoadAnalyzerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DetectorBase != "" {
		t.Errorf("expected empty DetectorBase, got %q", cfg.DetectorBase)
	}
	if cfg.DraftBase != "" {
		t.Errorf("expected empty DraftBase, got %q", cfg.DraftBase)
	}
}

func TestLoadAnalyzerDirMissingProfilesYAML(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadAnalyzerDir(dir); err == nil {
		t.Fatal("expected error for missing profiles.yaml")
	}
}

func TestLoadAnalyzerDirPromptFileResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
custom:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
  detector_prompt_file: prompts/detector.txt
  detector_prompt_extra_file: prompts/detector-extra.txt
  draft_prompt_file: prompts/draft.txt
  draft_prompt_extra_file: prompts/draft-extra.txt
`)
	writeFile(t, dir, "prompts/detector.txt", "custom detector prompt")
	writeFile(t, dir, "prompts/detector-extra.txt", "custom detector extra")
	writeFile(t, dir, "prompts/draft.txt", "custom draft prompt")
	writeFile(t, dir, "prompts/draft-extra.txt", "custom draft extra")

	cfg, err := LoadAnalyzerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["custom"]
	if p.DetectorPrompt != "custom detector prompt" {
		t.Errorf("expected DetectorPrompt resolved from file, got %q", p.DetectorPrompt)
	}
	if p.DetectorPromptExtra != "custom detector extra" {
		t.Errorf("expected DetectorPromptExtra resolved from file, got %q", p.DetectorPromptExtra)
	}
	if p.DraftPrompt != "custom draft prompt" {
		t.Errorf("expected DraftPrompt resolved from file, got %q", p.DraftPrompt)
	}
	if p.DraftPromptExtra != "custom draft extra" {
		t.Errorf("expected DraftPromptExtra resolved from file, got %q", p.DraftPromptExtra)
	}
	if p.DetectorPromptFile != "" || p.DetectorPromptExtraFile != "" ||
		p.DraftPromptFile != "" || p.DraftPromptExtraFile != "" {
		t.Error("expected all *_file fields cleared after resolution")
	}
}

func TestLoadAnalyzerDirBothSetError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
custom:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
  detector_prompt: inline prompt
  detector_prompt_file: prompts/detector.txt
`)
	writeFile(t, dir, "prompts/detector.txt", "file prompt")

	_, err := LoadAnalyzerDir(dir)
	if err == nil {
		t.Fatal("expected error when both inline and _file are set")
	}
}

func TestLoadAnalyzerDirRejectsTraversal(t *testing.T) {
	cases := []string{
		"/etc/passwd",
		"../outside.txt",
		"prompts/../../outside.txt",
		`prompts\detector.txt`,
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "profiles.yaml", fmt.Sprintf(`
custom:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
  detector_prompt_file: %q
`, ref))
			if _, err := LoadAnalyzerDir(dir); err == nil {
				t.Fatalf("expected error for path %q", ref)
			}
		})
	}
}

func TestLoadProfilesRejectsFileFields(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "profiles.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
custom:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
  detector_prompt_file: detector.txt
`), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadProfiles(yamlPath); err == nil {
		t.Fatal("expected LoadProfiles to reject *_prompt_file fields")
	}
}

// writeFile writes content to a file at dir/rel, creating parent
// directories as needed.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveDetectorPrompt(t *testing.T) {
	tests := []struct {
		name     string
		builtin  string
		profile  Profile
		expected string
	}{
		{
			name:     "all empty uses builtin",
			builtin:  "builtin prompt",
			profile:  Profile{},
			expected: "builtin prompt",
		},
		{
			name:    "DetectorPrompt replaces builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPrompt: "custom prompt",
			},
			expected: "custom prompt",
		},
		{
			name:    "DetectorPromptExtra appends to builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPromptExtra: "extra text",
			},
			expected: "builtin prompt\n\nextra text",
		},
		{
			name:    "DetectorPrompt and Extra: replace then append",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPrompt:      "custom prompt",
				DetectorPromptExtra: "extra text",
			},
			expected: "custom prompt\n\nextra text",
		},
		{
			name:    "Extra without replacement appends to builtin",
			builtin: "base text",
			profile: Profile{
				DetectorPromptExtra: "addition",
			},
			expected: "base text\n\naddition",
		},
		{
			name:    "DetectorPromptBase takes precedence over builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPromptBase: "analyzer-dir base",
			},
			expected: "analyzer-dir base",
		},
		{
			name:    "DetectorPrompt replacement takes precedence over DetectorPromptBase",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPromptBase: "analyzer-dir base",
				DetectorPrompt:     "profile replacement",
			},
			expected: "profile replacement",
		},
		{
			name:    "extra appends to DetectorPromptBase",
			builtin: "builtin prompt",
			profile: Profile{
				DetectorPromptBase:  "analyzer-dir base",
				DetectorPromptExtra: "extra text",
			},
			expected: "analyzer-dir base\n\nextra text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.profile.EffectiveDetectorPrompt(tt.builtin)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestEffectiveDraftPrompt(t *testing.T) {
	tests := []struct {
		name     string
		builtin  string
		profile  Profile
		expected string
	}{
		{
			name:     "all empty uses builtin",
			builtin:  "builtin prompt",
			profile:  Profile{},
			expected: "builtin prompt",
		},
		{
			name:    "DraftPrompt replaces builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPrompt: "custom prompt",
			},
			expected: "custom prompt",
		},
		{
			name:    "DraftPromptExtra appends to builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPromptExtra: "extra text",
			},
			expected: "builtin prompt\n\nextra text",
		},
		{
			name:    "DraftPrompt and Extra: replace then append",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPrompt:      "custom prompt",
				DraftPromptExtra: "extra text",
			},
			expected: "custom prompt\n\nextra text",
		},
		{
			name:    "DraftPromptBase takes precedence over builtin",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPromptBase: "analyzer-dir base",
			},
			expected: "analyzer-dir base",
		},
		{
			name:    "DraftPrompt replacement takes precedence over DraftPromptBase",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPromptBase: "analyzer-dir base",
				DraftPrompt:     "profile replacement",
			},
			expected: "profile replacement",
		},
		{
			name:    "extra appends to DraftPromptBase",
			builtin: "builtin prompt",
			profile: Profile{
				DraftPromptBase:  "analyzer-dir base",
				DraftPromptExtra: "extra text",
			},
			expected: "analyzer-dir base\n\nextra text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.profile.EffectiveDraftPrompt(tt.builtin)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestPromptHash(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
		isEmpty bool
	}{
		{
			name:    "all empty returns empty string",
			profile: Profile{},
			isEmpty: true,
		},
		{
			name: "DetectorPrompt non-empty",
			profile: Profile{
				DetectorPrompt: "custom",
			},
			isEmpty: false,
		},
		{
			name: "DetectorPromptExtra non-empty",
			profile: Profile{
				DetectorPromptExtra: "extra",
			},
			isEmpty: false,
		},
		{
			name: "DraftPrompt non-empty",
			profile: Profile{
				DraftPrompt: "custom",
			},
			isEmpty: false,
		},
		{
			name: "DraftPromptExtra non-empty",
			profile: Profile{
				DraftPromptExtra: "extra",
			},
			isEmpty: false,
		},
		{
			name: "all four non-empty",
			profile: Profile{
				DetectorPrompt:      "detector",
				DetectorPromptExtra: "detector extra",
				DraftPrompt:         "draft",
				DraftPromptExtra:    "draft extra",
			},
			isEmpty: false,
		},
		{
			name: "DetectorPromptBase alone is non-empty",
			profile: Profile{
				DetectorPromptBase: "base text",
			},
			isEmpty: false,
		},
		{
			name: "DraftPromptBase alone is non-empty",
			profile: Profile{
				DraftPromptBase: "base text",
			},
			isEmpty: false,
		},
		{
			name: "all six non-empty",
			profile: Profile{
				DetectorPromptBase:  "detector base",
				DetectorPrompt:      "detector",
				DetectorPromptExtra: "detector extra",
				DraftPromptBase:     "draft base",
				DraftPrompt:         "draft",
				DraftPromptExtra:    "draft extra",
			},
			isEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := tt.profile.PromptHash()
			if tt.isEmpty && hash != "" {
				t.Errorf("expected empty hash, got %q", hash)
			}
			if !tt.isEmpty && hash == "" {
				t.Error("expected non-empty hash")
			}
			// Check that hash is stable (hex format)
			if !tt.isEmpty && len(hash) != 64 {
				t.Errorf("expected sha256 hex (64 chars), got %d chars: %q", len(hash), hash)
			}
		})
	}

	// Test stability: same input should produce same hash
	p1 := Profile{
		DetectorPrompt:      "detector",
		DetectorPromptExtra: "extra",
	}
	p2 := Profile{
		DetectorPrompt:      "detector",
		DetectorPromptExtra: "extra",
	}
	if p1.PromptHash() != p2.PromptHash() {
		t.Error("expected same profile to produce same hash")
	}

	// Different profiles should produce different hashes (with high probability)
	p3 := Profile{
		DetectorPrompt: "different",
	}
	if p1.PromptHash() == p3.PromptHash() {
		t.Error("expected different profiles to produce different hashes")
	}

	// A base-only change (e.g. an analyzer-dir detector.md edit) must alter
	// the hash — that's the whole point of hashing the bases too.
	withBase := Profile{DetectorPromptBase: "detector.md v1"}
	withEditedBase := Profile{DetectorPromptBase: "detector.md v2"}
	if withBase.PromptHash() == withEditedBase.PromptHash() {
		t.Error("expected a base-only edit to change the hash")
	}
	empty := Profile{}
	if withBase.PromptHash() == empty.PromptHash() {
		t.Error("expected a base-only profile to differ from an all-empty profile")
	}
}
