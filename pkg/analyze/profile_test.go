// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestLoadProfiles(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(os.WriteFile(yamlPath, []byte(yamlContent), 0644))

	profiles, err := LoadProfiles(yamlPath)
	c.Require().NoError(err)

	c.Require().Len(profiles, 2, "expected 2 profiles, got %d", len(profiles))

	// Check basic profile
	basic, ok := profiles["basic"]
	c.Require().True(ok, "expected 'basic' profile")
	c.Eq("basic", basic.Name, "expected Name='basic', got")
	c.Eq("claude-opus-4-20250805", basic.DetectorModel, "expected DetectorModel='claude-opus-4-20250805', got")
	c.Eq("claude-haiku-4-5-20251001", basic.RankModel, "expected RankModel='claude-haiku-4-5-20251001', got")
	c.Eq("claude-opus-4-20250805", basic.DraftModel, "expected DraftModel='claude-opus-4-20250805', got")
	c.Eq(100, basic.Limit, "expected Limit=100, got")

	// Check minimal profile
	minimal, ok := profiles["minimal"]
	c.Require().True(ok, "expected 'minimal' profile")
	c.Eq("minimal", minimal.Name, "expected Name='minimal', got")
	c.Eq("gpt-4", minimal.DetectorModel, "expected DetectorModel='gpt-4', got")
}

func TestDefaults(t *testing.T) {
	c := assert.NewCollecting(t)
	p := &Profile{
		DetectorModel: "claude-opus-4-20250805",
		DraftModel:    "claude-opus-4-20250805",
	}
	p.Defaults()

	c.Eq(50, p.Limit, "expected Limit=50, got")
	c.Eq(16384, p.MaxOutputTokens, "expected MaxOutputTokens=16384, got")
	c.Eq(2048, p.Compact.MaxToolResultBytes, "expected MaxToolResultBytes=2048, got")
	c.Eq(300*1024, p.Compact.MaxTranscriptBytes, "expected MaxTranscriptBytes=307200, got")
	c.Eq(4, p.Compact.KeepFirstTurns, "expected KeepFirstTurns=4, got")
	c.Eq(20, p.Compact.KeepLastTurns, "expected KeepLastTurns=20, got")
}

func TestDefaultsPreservesExisting(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(75, p.Limit, "expected Limit=75 (preserved), got")
	c.Eq(8192, p.MaxOutputTokens, "expected MaxOutputTokens=8192 (preserved), got")
	c.Eq(4096, p.Compact.MaxToolResultBytes, "expected MaxToolResultBytes=4096 (preserved), got")
	c.Eq(500*1024, p.Compact.MaxTranscriptBytes, "expected MaxTranscriptBytes=512000 (preserved), got")
	c.Eq(2, p.Compact.KeepFirstTurns, "expected KeepFirstTurns=2 (preserved), got")
	c.Eq(30, p.Compact.KeepLastTurns, "expected KeepLastTurns=30 (preserved), got")
}

func TestUnknownFieldError(t *testing.T) {
	c := assert.NewAborting(t)
	yamlContent := `
test:
  detector_model: claude-opus-4-20250805
  draft_model: claude-opus-4-20250805
  unknown_field: invalid
`

	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "profiles.yaml")
	c.NoError(os.WriteFile(yamlPath, []byte(yamlContent), 0644))

	_, err := LoadProfiles(yamlPath)
	c.Error(err, "expected error for unknown field")
}

func TestCompactFieldsFromYAML(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(os.WriteFile(yamlPath, []byte(yamlContent), 0644))

	profiles, err := LoadProfiles(yamlPath)
	c.Require().NoError(err)

	custom := profiles["custom"]
	c.Eq(999, custom.Compact.MaxToolResultBytes, "expected MaxToolResultBytes=999, got")
	c.Eq(500000, custom.Compact.MaxTranscriptBytes, "expected MaxTranscriptBytes=500000, got")
	c.Eq(2, custom.Compact.KeepFirstTurns, "expected KeepFirstTurns=2, got")
	c.Eq(10, custom.Compact.KeepLastTurns, "expected KeepLastTurns=10, got")
}

func TestUnknownNestedFieldError(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(os.WriteFile(yamlPath, []byte(yamlContent), 0644))

	_, err := LoadProfiles(yamlPath)
	c.Error(err, "expected error for unknown nested field")
}

func TestLoadAnalyzerDir(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
default:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
`)
	writeFile(t, dir, "detector.md", "detector base text")
	writeFile(t, dir, "draft.md", "draft base text")

	cfg, err := LoadAnalyzerDir(dir)
	c.Require().NoError(err)
	c.Eq("detector base text", cfg.DetectorBase, "expected DetectorBase")
	c.Eq("draft base text", cfg.DraftBase, "expected DraftBase")
	p, ok := cfg.Profiles["default"]
	c.Require().True(ok, "expected 'default' profile")
	c.Eq("default", p.Name, "expected Name='default', got")
	// LoadAnalyzerDir must not itself attach the bases to profiles — that's
	// a resolution-layer concern.
	c.False(p.DetectorPromptBase != "" || p.DraftPromptBase != "", "expected LoadAnalyzerDir to leave *PromptBase unset on profiles, got %q / %q", p.DetectorPromptBase, p.DraftPromptBase)
}

func TestLoadAnalyzerDirMissingMdFilesOK(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeFile(t, dir, "profiles.yaml", `
default:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
`)

	cfg, err := LoadAnalyzerDir(dir)
	c.Require().NoError(err)
	c.Eq("", cfg.DetectorBase, "expected empty DetectorBase, got")
	c.Eq("", cfg.DraftBase, "expected empty DraftBase, got")
}

func TestLoadAnalyzerDirMissingProfilesYAML(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadAnalyzerDir(dir)
	assert.NewAborting(t).Error(err, "expected error for missing profiles.yaml")
}

func TestLoadAnalyzerDirPromptFileResolution(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err)
	p := cfg.Profiles["custom"]
	c.Eq("custom detector prompt", p.DetectorPrompt, "expected DetectorPrompt resolved from file, got")
	c.Eq("custom detector extra", p.DetectorPromptExtra, "expected DetectorPromptExtra resolved from file, got")
	c.Eq("custom draft prompt", p.DraftPrompt, "expected DraftPrompt resolved from file, got")
	c.Eq("custom draft extra", p.DraftPromptExtra, "expected DraftPromptExtra resolved from file, got")
	c.False(p.DetectorPromptFile != "" || p.DetectorPromptExtraFile != "" ||
		p.DraftPromptFile != "" || p.DraftPromptExtraFile != "", "expected all *_file fields cleared after resolution")
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
	assert.NewAborting(t).Error(err, "expected error when both inline and _file are set")
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
			_, err := LoadAnalyzerDir(dir)
			assert.NewAborting(t).Error(err, "expected error for path %q", ref)
		})
	}
}

func TestLoadProfilesRejectsFileFields(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "profiles.yaml")
	c.NoError(os.WriteFile(yamlPath, []byte(`
custom:
  detector_model: claude-sonnet-5
  draft_model: claude-sonnet-5
  detector_prompt_file: detector.txt
`), 0644))

	_, err := LoadProfiles(yamlPath)
	c.Error(err, "expected LoadProfiles to reject *_prompt_file fields")
}

// writeFile writes content to a file at dir/rel, creating parent
// directories as needed.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	c := assert.NewAborting(t)
	full := filepath.Join(dir, rel)
	c.NoError(os.MkdirAll(filepath.Dir(full), 0755))
	c.NoError(os.WriteFile(full, []byte(content), 0644))
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
			assert.NewCollecting(t).Eq(tt.expected, result, "expected")
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
			assert.NewCollecting(t).Eq(tt.expected, result, "expected")
		})
	}
}

func TestPromptHash(t *testing.T) {
	c := assert.NewCollecting(t)
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
			c := assert.NewCollecting(t)
			hash := tt.profile.PromptHash()
			c.False(tt.isEmpty && hash != "", "expected empty hash, got %q", hash)
			c.False(!tt.isEmpty && hash == "", "expected non-empty hash")
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
	c.Eq(p2.PromptHash(), p1.PromptHash(), "expected same profile to produce same hash")

	// Different profiles should produce different hashes (with high probability)
	p3 := Profile{
		DetectorPrompt: "different",
	}
	c.NotEq(p3.PromptHash(), p1.PromptHash(), "expected different profiles to produce different hashes")

	// A base-only change (e.g. an analyzer-dir detector.md edit) must alter
	// the hash — that's the whole point of hashing the bases too.
	withBase := Profile{DetectorPromptBase: "detector.md v1"}
	withEditedBase := Profile{DetectorPromptBase: "detector.md v2"}
	c.NotEq(withEditedBase.PromptHash(), withBase.PromptHash(), "expected a base-only edit to change the hash")
	empty := Profile{}
	c.NotEq(empty.PromptHash(), withBase.PromptHash(), "expected a base-only profile to differ from an all-empty profile")
}
