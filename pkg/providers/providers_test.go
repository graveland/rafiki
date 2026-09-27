// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(goodTOML))
	c.Require().NoError(err, "Parse")
	c.Eq("anthropic", set.DefaultProvider, "DefaultProvider")
	c.Require().Len(set.Providers, 3, "len(Providers) = %d, want 3", len(set.Providers))
	p, ok := set.Get("vmlx")
	c.Require().True(ok, "Get(vmlx) not found")
	c.Eq("vmlx", p.Name, "Name")
	c.Eq(providers.KindAnthropic, p.Kind, "Kind")
	c.Eq("http://localhost:8005", p.BaseURL, "BaseURL =")
	c.Eq("", p.APIKeyEnv, "APIKeyEnv")
	if got := set.Providers["anthropic"].Fallback; len(got) != 1 || got[0] != "openrouter" {
		t.Errorf("anthropic.Fallback = %v, want [openrouter]", got)
	}
	alias, ok := p.Models["qwen"]
	c.Require().True(ok, "vmlx.models.qwen not found")
	c.Eq("models/Qwen3.8-27B-Abliterated-MLX-4bit", alias.ID, "alias.ID =")
	c.Eq(16384, alias.ContextWindow, "alias.ContextWindow")
	c.Eq(3277, alias.ContextFilesTokens, "alias.ContextFilesTokens")
	c.False(alias.Skills == nil || *alias.Skills != "", "alias.Skills = %v, want a pointer to \"\"", alias.Skills)
	c.False(alias.MCPServers == nil || *alias.MCPServers != "codescan", "alias.MCPServers = %v, want a pointer to \"codescan\"", alias.MCPServers)
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
		{
			name: "only on a non-openrouter provider",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"anthropic\"\n[providers.x.models.qwen]\nid = \"m\"\nonly = [\"fireworks\"]\n",
			want: "models.qwen: only requires kind \"anthropic-openrouter\", not \"anthropic\"",
		},
		{
			name: "only on an openai provider",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"openai\"\nbase_url = \"http://localhost:8000\"\n[providers.x.models.g]\nid = \"m\"\nonly = [\"fireworks\"]\n",
			want: "models.g: only requires kind \"anthropic-openrouter\", not \"openai\"",
		},
		{
			name: "empty only slug",
			toml: "default_provider = \"x\"\n[providers.x]\nkind = \"anthropic-openrouter\"\n[providers.x.models.g]\nid = \"z-ai/glm-5.3-flash\"\nonly = [\"together\", \"\"]\n",
			want: "models.g: only must not contain an empty provider slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, err := providers.Parse([]byte(tc.toml))
			c.Require().Error(err, "Parse succeeded, want error containing %q", tc.want)
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}

// The shipped default must be valid and must preserve the two names already
// written into conversation_turn.upstream rows.
func TestDefaultIsValid(t *testing.T) {
	c := assert.NewCollecting(t)
	set := providers.Default()
	c.Require().NoError(set.Validate(), "Default() is invalid")
	for _, name := range []string{"anthropic", "openrouter"} {
		_, ok := set.Get(name)
		c.True(ok, "Default() is missing provider %q", name)
	}
	c.Eq("anthropic", set.DefaultProvider, "Default().DefaultProvider")
}

// A missing file is not an error: a fresh install has no providers.toml and
// must behave exactly as the shipped default.
func TestLoadMissingFileReturnsDefault(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Load(t.TempDir() + "/does-not-exist.toml")
	c.Require().NoError(err, "Load of a missing file")
	_, ok := set.Get("anthropic")
	c.True(ok, "Load of a missing file must return Default()")
}

func TestParseModelAliasSkillsUnsetVsEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Parse")
	p, _ := set.Get("x")
	c.Nil(p.Models["unset"].Skills, "unset: Skills")
	if got := p.Models["empty"].Skills; got == nil || *got != "" {
		t.Errorf("empty: Skills = %v, want pointer to \"\"", got)
	}
	if got := p.Models["star"].Skills; got == nil || *got != "*" {
		t.Errorf("star: Skills = %v, want pointer to \"*\"", got)
	}
}

// only is the alias-level OpenRouter provider pin: two aliases may name the
// SAME real id while pinning DIFFERENT provider slugs, which is what makes an
// A/B eval on one model possible. Absent = nil (no pin).
func TestParseModelAliasOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	const toml = `
default_provider = "openrouter"

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.openrouter.models."glm-flash@together"]
id   = "z-ai/glm-5.3-flash"
only = ["together"]

[providers.openrouter.models."glm-flash@fireworks"]
id   = "z-ai/glm-5.3-flash"
only = ["fireworks"]

[providers.openrouter.models.unpinned]
id = "z-ai/glm-5.2"
`
	set, err := providers.Parse([]byte(toml))
	c.Require().NoError(err, "Parse")
	p, ok := set.Get("openrouter")
	c.Require().True(ok, "Get(openrouter) not found")
	if got := p.Models["glm-flash@together"].Only; len(got) != 1 || got[0] != "together" {
		t.Errorf("glm-flash@together.Only = %v, want [together]", got)
	}
	if got := p.Models["glm-flash@fireworks"].Only; len(got) != 1 || got[0] != "fireworks" {
		t.Errorf("glm-flash@fireworks.Only = %v, want [fireworks]", got)
	}
	c.Nil(p.Models["unpinned"].Only, "unpinned.Only")
	// The two aliases above must still resolve to the same real id.
	c.Eq(p.Models["glm-flash@fireworks"].ID, p.Models["glm-flash@together"].ID, "two-aliases-one-id setup lost")
}

func TestProvidersLoadsEmbeddingsAndSummaries(t *testing.T) {
	t.Setenv("EMBED_TEST_API_KEY", "vk")
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Parse with [embeddings] and [summaries]")
	e := set.Embeddings
	c.Require().NotNil(e, "Embeddings = nil, want the decoded table")
	c.Eq("https://embed.internal:8443/v1/embeddings", e.URL, "Embeddings.URL =")
	c.Eq("EMBED_TEST_API_KEY", e.APIKeyEnv, "Embeddings.APIKeyEnv =")
	if e.Model != "bge-large" || e.Dimensions != 1536 {
		t.Errorf("Embeddings.Model = %q, Dimensions = %d, want bge-large, 1536", e.Model, e.Dimensions)
	}
	c.Eq("vk", e.APIKey(), "Embeddings.APIKey()")
	s := set.Summaries
	c.Require().NotNil(s, "Summaries = nil, want the decoded table")
	c.False(s.Model != "anthropic/claude-haiku-4-5" || s.MaxSegmentTokens != 6000, "Summaries = %+v, want model anthropic/claude-haiku-4-5, 6000", s)

	// A config without the tables must leave them nil (absent = BM25-only
	// recall, no summaries), and Undecoded must not reject the tables when
	// they ARE present — Parse above would have failed otherwise.
	plain, err := providers.Parse([]byte(goodTOML))
	c.Require().NoError(err, "Parse without the new tables")
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
			c := assert.NewCollecting(t)
			_, err := providers.Parse([]byte(base + tc.toml))
			c.Require().Error(err, "Parse succeeded, want error containing %q", tc.want)
			c.StrContains(err.Error(), tc.want, "error")
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
			c := assert.NewCollecting(t)
			_, err := providers.Parse([]byte(base + tc.toml))
			c.Require().Error(err, "Parse succeeded, want error containing %q", tc.want)
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}
