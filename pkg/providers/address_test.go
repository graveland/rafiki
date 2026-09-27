// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

const addrTOML = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
`

func TestSplit(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	cases := []struct {
		in           string
		wantProvider string
		wantModel    string
	}{
		{"anthropic/claude-sonnet-5", "anthropic", "claude-sonnet-5"},
		// Split on the FIRST slash only: everything after belongs to the
		// provider, slashes and all.
		{"openrouter/deepseek/deepseek-chat", "openrouter", "deepseek/deepseek-chat"},
		{"vmlx/models/Qwen3.8-27B-Abliterated-MLX-4bit", "vmlx", "models/Qwen3.8-27B-Abliterated-MLX-4bit"},
		// A bare id resolves against default_provider.
		{"claude-sonnet-5", "anthropic", "claude-sonnet-5"},
		// An alias substitutes its declared real id.
		{"vmlx/qwen", "vmlx", "models/Qwen3.8-27B-Abliterated-MLX-4bit"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			c := assert.NewCollecting(t)
			p, model, err := set.Split(tc.in)
			c.Require().NoError(err, "Split(%q)", tc.in)
			c.Eq(tc.wantProvider, p.Name, "provider")
			c.Eq(tc.wantModel, model, "model")
		})
	}
}

// The big-bang break: a first segment that is a VENDOR rather than a configured
// provider must error, never fall through to default_provider. Falling through
// would reintroduce shape-inference and make a typo'd provider name route
// somewhere plausible-looking instead of failing.
func TestSplitUnknownProviderErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	for _, in := range []string{"deepseek/deepseek-chat", "openai/gpt-4o", "google/gemini-2.5-pro"} {
		t.Run(in, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, _, err := set.Split(in)
			c.Require().Error(err, "Split(%q) succeeded; want an unknown-provider error", in)
			c.StrContains(err.Error(), "unknown provider", "error = %q, want it to contain \"unknown provider\"", err.Error())
			c.StrContains(err.Error(), "openrouter", "error")
		})
	}
}

func TestSplitEmptyErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	if _, _, err := set.Split(""); err == nil {
		t.Error("Split(\"\") succeeded; want an error")
	}
}

func TestSplitTrailingSlashErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	if _, _, err := set.Split("anthropic/"); err == nil {
		t.Error("Split(\"anthropic/\") succeeded; want an error for an empty model id")
	}
}

func TestSplitRaw(t *testing.T) {
	c := assert.NewCollecting(t)
	name, model := providers.SplitRaw("openrouter/deepseek/deepseek-chat")
	c.False(name != "openrouter" || model != "deepseek/deepseek-chat", "SplitRaw = (%q, %q), want (openrouter, deepseek/deepseek-chat)", name, model)
	name, model = providers.SplitRaw("claude-sonnet-5")
	c.False(name != "" || model != "claude-sonnet-5", "SplitRaw of a bare id = (%q, %q), want (\"\", claude-sonnet-5)", name, model)
}

const resolveTOML = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.openrouter.models."glm-flash@together"]
id   = "z-ai/glm-5.3-flash"
only = ["together"]

[providers.openrouter.models."glm-flash@fireworks"]
id   = "z-ai/glm-5.3-flash"
only = ["fireworks"]

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
`

// Resolve must return everything Split does PLUS the matched alias, so the
// alias's declared metadata (its only-pin) can travel with the request. The
// pin must come from the alias, never be looked up by real id: two aliases
// share one id and must keep their own pins apart.
func TestResolveReturnsAlias(t *testing.T) {
	set, err := providers.Parse([]byte(resolveTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	cases := []struct {
		in           string
		wantProvider string
		wantModel    string
		wantAlias    bool
		wantOnly     []string
	}{
		{"openrouter/glm-flash@together", "openrouter", "z-ai/glm-5.3-flash", true, []string{"together"}},
		{"openrouter/glm-flash@fireworks", "openrouter", "z-ai/glm-5.3-flash", true, []string{"fireworks"}},
		// A real (non-alias) id: no alias, no pin.
		{"openrouter/z-ai/glm-5.3-flash", "openrouter", "z-ai/glm-5.3-flash", false, nil},
		// An alias without only still resolves as an alias, with a nil pin.
		{"vmlx/qwen", "vmlx", "models/Qwen3.8-27B-Abliterated-MLX-4bit", true, nil},
		// A non-alias bare id against default_provider.
		{"claude-sonnet-5", "anthropic", "claude-sonnet-5", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			c := assert.NewCollecting(t)
			p, model, alias, err := set.Resolve(tc.in)
			c.Require().NoError(err, "Resolve(%q)", tc.in)
			c.Eq(tc.wantProvider, p.Name, "provider")
			c.Eq(tc.wantModel, model, "model")
			c.Require().False(tc.wantAlias && alias == nil, "alias = nil, want the matched alias")
			if !tc.wantAlias {
				c.Require().Nil(alias, "alias")
				return
			}
			got, want := alias.ID, tc.wantModel
			c.Eq(want, got, "alias.ID")
			c.Require().Len(alias.Only, len(tc.wantOnly), "alias.Only = %v, want %v", alias.Only, tc.wantOnly)
			for i := range tc.wantOnly {
				c.Eq(tc.wantOnly[i], alias.Only[i], "alias.Only[%d] = %q, want", i, alias.Only[i])
			}
		})
	}
}

func TestResolveUnknownProviderErrors(t *testing.T) {
	set, err := providers.Parse([]byte(resolveTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	if _, _, _, err := set.Resolve("deepseek/deepseek-chat"); err == nil {
		t.Error("Resolve of an unknown provider succeeded; want an error")
	}
}

// Split must behave EXACTLY as before now that it wraps Resolve: its callers
// (pkg/fundi, pkg/server, cmd/rafikid) were not taught about aliases and must
// not change behaviour.
func TestSplitMatchesResolve(t *testing.T) {
	set, err := providers.Parse([]byte(resolveTOML))
	assert.NewAborting(t).NoError(err, "Parse")
	for _, in := range []string{
		"openrouter/glm-flash@together",
		"openrouter/glm-flash@fireworks",
		"openrouter/z-ai/glm-5.3-flash",
		"vmlx/qwen",
		"claude-sonnet-5",
		"anthropic/claude-sonnet-5",
	} {
		t.Run(in, func(t *testing.T) {
			c := assert.NewCollecting(t)
			pSplit, modelSplit, errSplit := set.Split(in)
			pResolve, modelResolve, _, errResolve := set.Resolve(in)
			c.Require().Eq((errResolve == nil), (errSplit == nil), "Split err = %v, Resolve err = %v; must agree", errSplit, errResolve)
			if errSplit != nil {
				return
			}
			c.False(pSplit.Name != pResolve.Name || modelSplit != modelResolve, "Split = (%q, %q), Resolve = (%q, %q); must agree", pSplit.Name, modelSplit, pResolve.Name, modelResolve)
		})
	}
}
