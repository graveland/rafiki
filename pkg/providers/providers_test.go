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

[providers.vmlx.models.qwen]
id                   = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window       = 16384
context_files_tokens = 3277
skills                = ""
mcp_servers           = "codescan"
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
	alias, ok := p.Models["qwen"]
	if !ok {
		t.Fatal("vmlx.models.qwen not found")
	}
	if alias.ID != "models/Qwen3.8-27B-Abliterated-MLX-4bit" {
		t.Errorf("alias.ID = %q", alias.ID)
	}
	if alias.ContextWindow != 16384 {
		t.Errorf("alias.ContextWindow = %d, want 16384", alias.ContextWindow)
	}
	if alias.ContextFilesTokens != 3277 {
		t.Errorf("alias.ContextFilesTokens = %d, want 3277", alias.ContextFilesTokens)
	}
	if alias.Skills == nil || *alias.Skills != "" {
		t.Errorf("alias.Skills = %v, want a pointer to \"\"", alias.Skills)
	}
	if alias.MCPServers == nil || *alias.MCPServers != "codescan" {
		t.Errorf("alias.MCPServers = %v, want a pointer to \"codescan\"", alias.MCPServers)
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
		{
			name: "model alias with no id",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"anthropic\"\n[providers.x.models.qwen]\ncontext_window = 16384\n",
			want: "models.qwen: id is required",
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

func TestParseModelAliasSkillsUnsetVsEmpty(t *testing.T) {
	const toml = `
default_provider = "x"
[providers.x]
kind = "anthropic"
[providers.x.models.unset]
id = "m1"
[providers.x.models.empty]
id = "m2"
skills = ""
[providers.x.models.star]
id = "m3"
skills = "*"
`
	set, err := providers.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, _ := set.Get("x")
	if got := p.Models["unset"].Skills; got != nil {
		t.Errorf("unset: Skills = %v, want nil (key absent)", got)
	}
	if got := p.Models["empty"].Skills; got == nil || *got != "" {
		t.Errorf("empty: Skills = %v, want pointer to \"\"", got)
	}
	if got := p.Models["star"].Skills; got == nil || *got != "*" {
		t.Errorf("star: Skills = %v, want pointer to \"*\"", got)
	}
}

func TestProvidersLoadsEmbeddingsAndSummaries(t *testing.T) {
	t.Setenv("EMBED_TEST_API_KEY", "vk")
	const toml = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[embeddings]
url = "https://embed.internal:8443/v1/embeddings"
api_key_env = "EMBED_TEST_API_KEY"
model = "bge-large"
dimensions = 1536

[summaries]
model = "anthropic/claude-haiku-4-5"
max_segment_tokens = 6000
`
	set, err := providers.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse with [embeddings] and [summaries]: %v", err)
	}
	e := set.Embeddings
	if e == nil {
		t.Fatal("Embeddings = nil, want the decoded table")
	}
	if e.URL != "https://embed.internal:8443/v1/embeddings" {
		t.Errorf("Embeddings.URL = %q", e.URL)
	}
	if e.APIKeyEnv != "EMBED_TEST_API_KEY" {
		t.Errorf("Embeddings.APIKeyEnv = %q", e.APIKeyEnv)
	}
	if e.Model != "bge-large" || e.Dimensions != 1536 {
		t.Errorf("Embeddings.Model = %q, Dimensions = %d, want bge-large, 1536", e.Model, e.Dimensions)
	}
	if got := e.APIKey(); got != "vk" {
		t.Errorf("Embeddings.APIKey() = %q, want the env value", got)
	}
	s := set.Summaries
	if s == nil {
		t.Fatal("Summaries = nil, want the decoded table")
	}
	if s.Model != "anthropic/claude-haiku-4-5" || s.MaxSegmentTokens != 6000 {
		t.Errorf("Summaries = %+v, want model anthropic/claude-haiku-4-5, 6000", s)
	}

	// A config without the tables must leave them nil (absent = BM25-only
	// recall, no summaries), and Undecoded must not reject the tables when
	// they ARE present — Parse above would have failed otherwise.
	plain, err := providers.Parse([]byte(goodTOML))
	if err != nil {
		t.Fatalf("Parse without the new tables: %v", err)
	}
	if plain.Embeddings != nil || plain.Summaries != nil {
		t.Errorf("tables must be nil when absent: Embeddings = %v, Summaries = %v", plain.Embeddings, plain.Summaries)
	}
}

func TestProvidersRejectsBadEmbeddings(t *testing.T) {
	const base = `
default_provider = "x"
[providers.x]
kind = "anthropic"
`
	const embeddings = `
[embeddings]
url = "https://e.internal/v1"
model = "m"
dimensions = 1536
`
	cases := []struct {
		name string
		toml string
		want string // substring the error must contain
	}{
		{
			name: "dimensions zero",
			toml: strings.ReplaceAll(embeddings, "dimensions = 1536", ""),
			want: "[embeddings] dimensions must be 1..16000, got 0",
		},
		{
			name: "dimensions too large",
			toml: strings.ReplaceAll(embeddings, "dimensions = 1536", "dimensions = 16001"),
			want: "[embeddings] dimensions must be 1..16000, got 16001",
		},
		{
			name: "relative url",
			toml: strings.ReplaceAll(embeddings, "https://e.internal/v1", "embed.internal/v1"),
			want: "[embeddings] url must be an absolute http(s) URL",
		},
		{
			name: "url without a host",
			toml: strings.ReplaceAll(embeddings, "https://e.internal/v1", "http://"),
			want: "[embeddings] url must be an absolute http(s) URL",
		},
		{
			name: "wrong scheme",
			toml: strings.ReplaceAll(embeddings, "https://e.internal/v1", "ftp://e.internal/v1"),
			want: "[embeddings] url must be an absolute http(s) URL",
		},
		{
			name: "missing model",
			toml: strings.ReplaceAll(embeddings, "model = \"m\"\n", ""),
			want: "[embeddings] model is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := providers.Parse([]byte(base + tc.toml))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestProvidersRejectsBadSummaries(t *testing.T) {
	const base = `
default_provider = "x"
[providers.x]
kind = "anthropic"
`
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "missing model",
			toml: "[summaries]\nmax_segment_tokens = 6000\n",
			want: "[summaries] model is required",
		},
		{
			name: "negative max_segment_tokens",
			toml: "[summaries]\nmodel = 'anthropic/claude-haiku-4-5'\nmax_segment_tokens = -1\n",
			want: "[summaries] max_segment_tokens must be >= 0, got -1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := providers.Parse([]byte(base + tc.toml))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}
