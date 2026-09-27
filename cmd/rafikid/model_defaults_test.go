// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

func TestContextFilesBudget(t *testing.T) {
	cases := []struct {
		name          string
		contextWindow int
		want          int
	}{
		{"unknown window", 0, 0},
		{"negative window", -1, 0},
		{"below the floor", 4000, 1024},  // 4000/5=800, clamped up to 1024
		{"mid-range", 16384, 3276},       // 16384/5=3276 (integer division)
		{"above the cap", 200000, 30000}, // 200000/5=40000, clamped down to 30000
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contextFilesBudget(tc.contextWindow)
			assert.NewCollecting(t).Eq(tc.want, got, "contextFilesBudget(%d) = %d, want", tc.contextWindow, got)
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

[providers.vmlx.models.explicitbudget]
id                   = "models/Explicit"
context_window       = 61440
context_files_tokens = 4096
`

func TestResolveModelDefaults_UsesAliasOverridesAndFormula(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	c.Require().NoError(err, "Parse")

	got, ok := resolveModelDefaults(set, "vmlx/qwen")
	c.Require().True(ok, "expected ok=true for a declared alias")
	c.Eq(3276, got.ContextFilesTokens, "ContextFilesTokens")
	c.False(got.Skills == nil || *got.Skills != "", "Skills = %v, want pointer to \"\"", got.Skills)
	c.False(got.MCPServers == nil || *got.MCPServers != "codescan", "MCPServers = %v, want pointer to \"codescan\"", got.MCPServers)
}

func TestResolveModelDefaults_NoSkillsFieldLeavesNilOverride(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	c.Require().NoError(err, "Parse")
	got, ok := resolveModelDefaults(set, "vmlx/noskillsoverride")
	c.Require().True(ok, "expected ok=true")
	c.Nil(got.Skills, "Skills")
	c.Eq(13107, got.ContextFilesTokens, "ContextFilesTokens")
}

// TestResolveModelDefaults_ExplicitContextFilesTokensWins covers the half of
// resolveModelDefaults' budget branch that the auto-formula cases cannot: an
// alias that declares context_files_tokens must get that number verbatim, not
// the formula's. This is the path the shipped configuration actually takes —
// every alias tuned by hand sets the field — so leaving it unexercised means
// the deployed behaviour is the untested one.
func TestResolveModelDefaults_ExplicitContextFilesTokensWins(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	c.Require().NoError(err, "Parse")
	got, ok := resolveModelDefaults(set, "vmlx/explicitbudget")
	c.Require().True(ok, "expected ok=true for a declared alias")
	c.Eq(4096, got.ContextFilesTokens, "ContextFilesTokens = %d, want the declared 4096, not the formula's %d", got.ContextFilesTokens, contextFilesBudget(61440))
	// Sanity: the fixture is only meaningful if the formula would disagree —
	// 61440/5 is 12288, so an accidental formula result cannot pass above.
	c.Require().NotEq(4096, contextFilesBudget(61440), "fixture is useless: the auto formula happens to equal the explicit value")
}

func TestResolveModelDefaults_UnaliasedModelIsNotOK(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(modelDefaultsTOML))
	c.Require().NoError(err, "Parse")
	_, ok := resolveModelDefaults(set, "anthropic/claude-sonnet-5")
	c.False(ok, "expected ok=false: anthropic/claude-sonnet-5 names no declared alias")
}

func TestResolveModelDefaults_NilSetIsNotOK(t *testing.T) {
	_, ok := resolveModelDefaults(nil, "vmlx/qwen")
	assert.NewCollecting(t).False(ok, "expected ok=false for a nil set")
}
