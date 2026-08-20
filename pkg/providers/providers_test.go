// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
)

const goodTOML = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"
fallback = ["openrouter"]

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"
`

func TestParseGood(t *testing.T) {
	set, err := providers.Parse([]byte(goodTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if set.DefaultProvider != "anthropic" {
		t.Errorf("DefaultProvider = %q, want %q", set.DefaultProvider, "anthropic")
	}
	if len(set.Providers) != 3 {
		t.Fatalf("len(Providers) = %d, want 3", len(set.Providers))
	}
	p, ok := set.Get("vmlx")
	if !ok {
		t.Fatal("Get(vmlx) not found")
	}
	if p.Name != "vmlx" {
		t.Errorf("Name = %q, want vmlx (Load must stamp the map key onto the struct)", p.Name)
	}
	if p.Kind != providers.KindAnthropic {
		t.Errorf("Kind = %q, want %q", p.Kind, providers.KindAnthropic)
	}
	if p.BaseURL != "http://localhost:8005" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if p.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty: vmlx is keyless", p.APIKeyEnv)
	}
	if got := set.Providers["anthropic"].Fallback; len(got) != 1 || got[0] != "openrouter" {
		t.Errorf("anthropic.Fallback = %v, want [openrouter]", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string // substring the error must contain
	}{
		{
			name: "illegal provider name",
			toml: "default_provider = \"A\"\n[providers.A]\nkind = \"anthropic\"\n",
			want: "invalid provider name",
		},
		{
			name: "name with a slash",
			toml: "default_provider = \"a/b\"\n[providers.\"a/b\"]\nkind = \"anthropic\"\n",
			want: "invalid provider name",
		},
		{
			name: "unknown kind",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"bedrock\"\n",
			want: "unknown kind",
		},
		{
			name: "missing default_provider",
			toml: "[providers.x]\nkind = \"anthropic\"\n",
			want: "default_provider",
		},
		{
			name: "default_provider names nothing",
			toml: "default_provider = \"nope\"\n[providers.x]\nkind = \"anthropic\"\n",
			want: "default_provider \"nope\"",
		},
		{
			name: "fallback names nothing",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"anthropic\"\nfallback = [\"ghost\"]\n",
			want: "fallback target \"ghost\"",
		},
		{
			name: "no providers at all",
			toml: "default_provider = \"x\"\n",
			want: "no providers",
		},
		{
			name: "extras.provider is reserved",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"anthropic-openrouter\"\n[providers.x.extras]\nprovider = { sort = \"price\" }\n",
			want: "extras.provider is reserved",
		},
		{
			name: "unknown top-level key",
			toml: "default_provider = \"x\"\nnonsense = 1\n[providers.x]\nkind = \"anthropic\"\n",
			want: "unknown key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := providers.Parse([]byte(tc.toml))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// The shipped default must be valid and must preserve the two names already
// written into conversation_turn.upstream rows.
func TestDefaultIsValid(t *testing.T) {
	set := providers.Default()
	if err := set.Validate(); err != nil {
		t.Fatalf("Default() is invalid: %v", err)
	}
	for _, name := range []string{"anthropic", "openrouter"} {
		if _, ok := set.Get(name); !ok {
			t.Errorf("Default() is missing provider %q", name)
		}
	}
	if set.DefaultProvider != "anthropic" {
		t.Errorf("Default().DefaultProvider = %q, want anthropic", set.DefaultProvider)
	}
}

// A missing file is not an error: a fresh install has no providers.toml and
// must behave exactly as the shipped default.
func TestLoadMissingFileReturnsDefault(t *testing.T) {
	set, err := providers.Load(t.TempDir() + "/does-not-exist.toml")
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if _, ok := set.Get("anthropic"); !ok {
		t.Error("Load of a missing file must return Default()")
	}
}
