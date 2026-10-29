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
	decl, ok := p.Models["qwen"]
	c.Require().True(ok, "vmlx.models.qwen not found")
	c.Eq("models/Qwen3.8-27B-Abliterated-MLX-4bit", decl.ID, "decl.ID =")
	c.Eq(16384, decl.ContextWindow, "decl.ContextWindow")
	c.Eq(3277, decl.ContextFilesTokens, "decl.ContextFilesTokens")
	c.False(decl.Skills == nil || *decl.Skills != "", "decl.Skills = %v, want a pointer to \"\"", decl.Skills)
	c.False(decl.MCPServers == nil || *decl.MCPServers != "codescan", "decl.MCPServers = %v, want a pointer to \"codescan\"", decl.MCPServers)
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
			name: "declared model with no id",
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

func TestParseModelDeclSkillsUnsetVsEmpty(t *testing.T) {
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

// only is the declaration-level OpenRouter provider pin: two declarations may
// name the SAME real id while pinning DIFFERENT provider slugs, which is what
// makes an A/B eval on one model possible. Absent = nil (no pin).
func TestParseModelDeclOnly(t *testing.T) {
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
	// The two declarations above must still resolve to the same real id.
	c.Eq(p.Models["glm-flash@fireworks"].ID, p.Models["glm-flash@together"].ID, "two-declarations-one-id setup lost")
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

// The ChatGPT-subscription kind owns its endpoint and its credential: a key in
// the environment would shadow the owner's login, via_executor would put the
// bearer on someone else's host, and a base_url would point at a different
// service. Each is refused by name so the fix is in the editor, not a
// 401 from a request that looked configured.
func TestValidateChatGPTKindRefusesAPIKeyEnv(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := providers.Parse([]byte(`
default_provider = "chatgpt"
[providers.chatgpt]
kind = "openai-responses-chatgpt"
api_key_env = "OPENAI_API_KEY"
`))
	c.Require().Error(err, "Parse accepted api_key_env on kind openai-responses-chatgpt")
	c.Eq("providers: provider \"chatgpt\": kind \"openai-responses-chatgpt\" authenticates with the owner's ChatGPT login (rafiki providers login chatgpt); remove api_key_env", err.Error(), "error")
}

func TestValidateChatGPTKindRefusesViaExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := providers.Parse([]byte(`
default_provider = "chatgpt"
[providers.chatgpt]
kind = "openai-responses-chatgpt"
[providers.chatgpt.via_executor]
selector = "pool"
proxy = "http://127.0.0.1:8080"
`))
	c.Require().Error(err, "Parse accepted via_executor on kind openai-responses-chatgpt")
	c.Eq("providers: provider \"chatgpt\": kind \"openai-responses-chatgpt\" does not support via_executor (the credential never leaves the daemon)", err.Error(), "error")
}

func TestValidateChatGPTKindRefusesBaseURL(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := providers.Parse([]byte(`
default_provider = "chatgpt"
[providers.chatgpt]
kind = "openai-responses-chatgpt"
base_url = "https://example.test/v1"
`))
	c.Require().Error(err, "Parse accepted base_url on kind openai-responses-chatgpt")
	c.Eq("providers: provider \"chatgpt\": kind \"openai-responses-chatgpt\" has a fixed base_url; remove it", err.Error(), "error")
}

// The api-key Responses kind is an ordinary endpoint: it may be reached
// through an executor (base_url is the target either way) and it may name its
// own base_url, since api.openai.com is only the default.
func TestValidateResponsesKindAllowsViaExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(`
default_provider = "oa"
[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
base_url = "https://api.openai.com/v1"
[providers.oa.via_executor]
selector = "pool"
proxy = "http://127.0.0.1:8080"
`))
	c.Require().NoError(err, "Parse rejected a valid openai-responses provider")
	p, ok := set.Get("oa")
	c.Require().True(ok, "Get(oa) not found")
	c.Require().True(p.ViaExecutor != nil, "ViaExecutor was dropped")
	c.Eq("http://127.0.0.1:8080", p.ViaExecutor.Proxy, "ViaExecutor.Proxy")
	c.Eq("https://api.openai.com/v1", p.BaseURL, "BaseURL")
}

func TestValidateResponsesKindAllowsNoBaseURL(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(`
default_provider = "oa"
[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
`))
	c.Require().NoError(err, "Parse rejected openai-responses with no base_url")
	p, _ := set.Get("oa")
	c.Eq("", p.BaseURL, "BaseURL =")
	c.Eq(providers.KindOpenAIResponses, p.Kind, "Kind")
}

// TestCatalogModelIDForEveryKind pins the shared mapping: only the two
// Responses kinds rewrite a provider-local id, they leave an already qualified
// id alone, and every other kind passes its id through untouched. A mapping
// that widened to an existing kind would silently reprice or re-window models
// that were catalog-shaped all along.
func TestCatalogModelIDForEveryKind(t *testing.T) {
	c := assert.NewCollecting(t)
	const (
		bare    = "gpt-6.1-sol"
		slashed = "openai/gpt-6.1-sol"
	)
	for _, kind := range []providers.Kind{providers.KindOpenAIResponses, providers.KindOpenAIResponsesChatGPT} {
		c.Eq(slashed, providers.CatalogModelID(kind, bare), "CatalogModelID(%q, bare)", kind)
		c.Eq(slashed, providers.CatalogModelID(kind, slashed), "CatalogModelID(%q, slashed) must not be prefixed twice", kind)
	}
	c.Eq(bare, providers.CatalogModelID(providers.KindOpenAI, bare), "KindOpenAI is not rewritten")
	c.Eq("claude-sonnet-5", providers.CatalogModelID(providers.KindAnthropic, "claude-sonnet-5"), "KindAnthropic is not rewritten")
	c.Eq("z-ai/glm-5.3-flash", providers.CatalogModelID(providers.KindAnthropicOpenRouter, "z-ai/glm-5.3-flash"), "KindAnthropicOpenRouter is not rewritten")
	c.Eq(bare, providers.CatalogModelID(providers.Kind(""), bare), "an empty kind is not rewritten")
}

// TestCatalogModelIDsFromConfiguredKinds pins the candidate list a cost rollup
// offers the catalog for a stored model id: the id itself first, then only the
// shapes this configuration's kinds could have produced. A set with no
// Responses kind must add nothing, which is what keeps every existing provider
// priced exactly as before.
func TestCatalogModelIDsFromConfiguredKinds(t *testing.T) {
	c := assert.NewCollecting(t)

	set, err := providers.Parse([]byte(`
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"

[providers.chatgpt]
kind = "openai-responses-chatgpt"
`))
	c.Require().NoError(err, "Parse")
	c.EqDeep([]string{"gpt-6.1-sol", "openai/gpt-6.1-sol"}, set.CatalogIDs("gpt-6.1-sol"),
		"both Responses kinds map to one candidate, deduped")
	c.EqDeep([]string{"openai/gpt-6.1-sol"}, set.CatalogIDs("openai/gpt-6.1-sol"),
		"an already-qualified id adds no candidate")

	// The shipped default has no Responses kind: one candidate, the id itself.
	c.EqDeep([]string{"claude-sonnet-5"}, providers.Default().CatalogIDs("claude-sonnet-5"),
		"a catalog-shaped-only set adds nothing")
}

// TestCatalogModelIDsMapsAQualifiedIDThroughItsProvider pins the arm a stored
// row needs when it DOES name its provider: the messages proxy records the id
// it resolved, which for a spawn that named "oa" is "oa/gpt-6.1-sol". That
// string never resolves against the catalog on its own (CatalogModelID leaves a
// slashed id alone), so the qualified arm strips the provider name and rewrites
// the remainder by THAT provider's kind. The raw id stays first, and the
// stripped candidate is offered only when a configured provider rewrites the
// remainder: an Anthropic provider maps to the identity, an unknown first
// segment maps to nothing, and a catalog-shaped provider
// ("openrouter/deepseek/deepseek-chat") must never be stripped into a
// different, wrong id.
func TestCatalogModelIDsMapsAQualifiedIDThroughItsProvider(t *testing.T) {
	c := assert.NewCollecting(t)

	parse := func(t *testing.T, body string) *providers.Set {
		t.Helper()
		set, err := providers.Parse([]byte(body))
		c.Require().NoError(err, "Parse")
		return set
	}

	responses := parse(t, `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
`)

	// oa is a Responses provider: the prefix comes off and the remainder is
	// rewritten to the catalog id, offered AFTER the raw id.
	c.EqDeep([]string{"oa/gpt-6.1-sol", "openai/gpt-6.1-sol"}, responses.CatalogIDs("oa/gpt-6.1-sol"),
		"a qualified Responses-kind id must offer its catalog id")

	// The qualified arm must not change what a BARE id offers: the same list
	// as before, and in the same order.
	c.EqDeep([]string{"gpt-6.1-sol", "openai/gpt-6.1-sol"}, responses.CatalogIDs("gpt-6.1-sol"),
		"a bare id is unchanged")

	// oa as an Anthropic provider rewrites nothing, so stripping its prefix
	// would only offer the bare id, which is a different model's address.
	anthropicOa := parse(t, `
default_provider = "oa"

[providers.oa]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"
`)
	c.EqDeep([]string{"oa/gpt-6.1-sol"}, anthropicOa.CatalogIDs("oa/gpt-6.1-sol"),
		"an Anthropic provider adds no stripped candidate")

	// An unknown first segment names no configured provider.
	c.EqDeep([]string{"zz/gpt-6.1-sol"}, responses.CatalogIDs("zz/gpt-6.1-sol"),
		"an unknown provider segment adds nothing")

	// A catalog-shaped provider's ids are already catalog ids.
	catalogShaped := parse(t, `
default_provider = "openrouter"

[providers.openrouter]
kind = "anthropic-openrouter"
base_url = "https://openrouter.ai/api"
`)
	c.EqDeep([]string{"openrouter/deepseek/deepseek-chat"}, catalogShaped.CatalogIDs("openrouter/deepseek/deepseek-chat"),
		"a catalog-shaped provider must not have its prefix stripped")
}
