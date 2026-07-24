package analyze

import (
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
		DetectorModel: "claude-opus-4-20250805",
		DraftModel:    "claude-opus-4-20250805",
		Limit:         75,
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
}
