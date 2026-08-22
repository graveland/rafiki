// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
)

func TestContextFilesBudget(t *testing.T) {
	cases := []struct {
		name          string
		contextWindow int
		want          int
	}{
		{"unknown window", 0, 0},
		{"negative window", -1, 0},
		{"below the floor", 4000, 1024},   // 4000/5=800, clamped up to 1024
		{"mid-range", 16384, 3276},        // 16384/5=3276 (integer division)
		{"above the cap", 200000, 30000},  // 200000/5=40000, clamped down to 30000
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextFilesBudget(tc.contextWindow); got != tc.want {
				t.Errorf("contextFilesBudget(%d) = %d, want %d", tc.contextWindow, got, tc.want)
			}
		})
	}
}

const modelDefaultsTOML = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id             = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
skills         = ""
mcp_servers    = "codescan"

[providers.vmlx.models.noskillsoverride]
id             = "models/Other"
context_window = 65536
`

func TestResolveModelDefaults_UsesAliasOverridesAndFormula(t *testing.T) {
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, ok := resolveModelDefaults(set, "vmlx/qwen")
	if !ok {
		t.Fatal("expected ok=true for a declared alias")
	}
	if got.ContextFilesTokens != 3276 {
		t.Errorf("ContextFilesTokens = %d, want 3276 (auto formula: 16384/5)", got.ContextFilesTokens)
	}
	if got.Skills == nil || *got.Skills != "" {
		t.Errorf("Skills = %v, want pointer to \"\"", got.Skills)
	}
	if got.MCPServers == nil || *got.MCPServers != "codescan" {
		t.Errorf("MCPServers = %v, want pointer to \"codescan\"", got.MCPServers)
	}
}

func TestResolveModelDefaults_NoSkillsFieldLeavesNilOverride(t *testing.T) {
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, ok := resolveModelDefaults(set, "vmlx/noskillsoverride")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Skills != nil {
		t.Errorf("Skills = %v, want nil (field not set in TOML)", got.Skills)
	}
	if got.ContextFilesTokens != 13107 {
		t.Errorf("ContextFilesTokens = %d, want 13107 (65536/5)", got.ContextFilesTokens)
	}
}

func TestResolveModelDefaults_UnaliasedModelIsNotOK(t *testing.T) {
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := resolveModelDefaults(set, "anthropic/claude-sonnet-5"); ok {
		t.Error("expected ok=false: anthropic/claude-sonnet-5 names no declared alias")
	}
}

func TestResolveModelDefaults_NilSetIsNotOK(t *testing.T) {
	if _, ok := resolveModelDefaults(nil, "vmlx/qwen"); ok {
		t.Error("expected ok=false for a nil set")
	}
}
