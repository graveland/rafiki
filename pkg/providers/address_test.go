// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
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
`

func TestSplit(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
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
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, model, err := set.Split(tc.in)
			if err != nil {
				t.Fatalf("Split(%q): %v", tc.in, err)
			}
			if p.Name != tc.wantProvider {
				t.Errorf("provider = %q, want %q", p.Name, tc.wantProvider)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

// The big-bang break: a first segment that is a VENDOR rather than a configured
// provider must error, never fall through to default_provider. Falling through
// would reintroduce shape-inference and make a typo'd provider name route
// somewhere plausible-looking instead of failing.
func TestSplitUnknownProviderErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, in := range []string{"deepseek/deepseek-chat", "openai/gpt-4o", "google/gemini-2.5-pro"} {
		t.Run(in, func(t *testing.T) {
			_, _, err := set.Split(in)
			if err == nil {
				t.Fatalf("Split(%q) succeeded; want an unknown-provider error", in)
			}
			if !strings.Contains(err.Error(), "unknown provider") {
				t.Errorf("error = %q, want it to contain \"unknown provider\"", err.Error())
			}
			if !strings.Contains(err.Error(), "openrouter") {
				t.Errorf("error = %q, want it to list the configured names so the fix is obvious", err.Error())
			}
		})
	}
}

func TestSplitEmptyErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := set.Split(""); err == nil {
		t.Error("Split(\"\") succeeded; want an error")
	}
}

func TestSplitTrailingSlashErrors(t *testing.T) {
	set, err := providers.Parse([]byte(addrTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := set.Split("anthropic/"); err == nil {
		t.Error("Split(\"anthropic/\") succeeded; want an error for an empty model id")
	}
}

func TestSplitRaw(t *testing.T) {
	name, model := providers.SplitRaw("openrouter/deepseek/deepseek-chat")
	if name != "openrouter" || model != "deepseek/deepseek-chat" {
		t.Errorf("SplitRaw = (%q, %q), want (openrouter, deepseek/deepseek-chat)", name, model)
	}
	name, model = providers.SplitRaw("claude-sonnet-5")
	if name != "" || model != "claude-sonnet-5" {
		t.Errorf("SplitRaw of a bare id = (%q, %q), want (\"\", claude-sonnet-5)", name, model)
	}
}
