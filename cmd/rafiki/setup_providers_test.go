// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// providersFixture isolates everything runProvidersStep can touch — the
// profile/config dirs, providers.toml (via RAFIKI_PROVIDERS), Ollama (a closed
// port), and the key-validation endpoints (a local 200 server) — so no test
// reaches real state.
type providersFixture struct {
	t    *testing.T
	cmd  *cobra.Command
	w    *wizard
	r    *fakeRunner
	out  *bytes.Buffer
	path string
	st   installstate.State
}

func newProvidersFixture(t *testing.T) *providersFixture {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.toml")
	t.Setenv("RAFIKI_PROVIDERS", path)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// ServiceEnvFile() honours RAFIKI_ENV_FILE first; blank it so a key never
	// reaches a developer's real service.env.
	t.Setenv(paths.EnvFile, "")

	origOllama := ollamaBaseURL
	ollamaBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { ollamaBaseURL = origOllama })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	origKeys := keyCheckBaseURLs
	t.Cleanup(func() { keyCheckBaseURLs = origKeys })
	keyCheckBaseURLs = map[string]string{"openrouter": srv.URL, "anthropic": srv.URL}

	cmd := &cobra.Command{Use: "setup"}
	cmd.Flags().Bool("non-interactive", false, "")
	cmd.Flags().String(keyOpenRouterFlag, "", "")
	cmd.Flags().String(keyAnthropicFlag, "", "")
	c.NoError(cmd.Flags().Set("non-interactive", "true"), "set non-interactive")

	out := &bytes.Buffer{}
	w := newWizard(cmd)
	w.in = &bytes.Buffer{}
	w.out = out
	w.interactive = false

	return &providersFixture{t: t, cmd: cmd, w: w, r: newFakeRunner(), out: out, path: path}
}

func (f *providersFixture) run(savedEnv map[string]string) (providersResult, error) {
	if savedEnv == nil {
		savedEnv = map[string]string{}
	}
	return runProvidersStep(context.Background(), f.w, f.r, f.out, &f.st, savedEnv)
}

func (f *providersFixture) fileBytes() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.path)
	assert.NewAborting(f.t).NoError(err, "read providers.toml")
	return b
}

func (f *providersFixture) parse() *providers.Set {
	f.t.Helper()
	set, err := providers.Parse(f.fileBytes())
	assert.NewAborting(f.t).NoError(err, "parse providers.toml")
	return set
}

func (f *providersFixture) backups() []string {
	f.t.Helper()
	names, err := filepath.Glob(f.path + ".*.bak")
	assert.NewAborting(f.t).NoError(err, "glob backups")
	return names
}

func (f *providersFixture) serviceEnv() string {
	f.t.Helper()
	b, _ := os.ReadFile(paths.ServiceEnvFile())
	return string(b)
}

func TestProvidersStepOpenRouterOnly(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	c.NoError(f.cmd.Flags().Set(keyOpenRouterFlag, "sk-good"), "set key flag")

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.True(res.Changed, "a written file and env change must report Changed")
	c.EqDeep([]string{"openrouter"}, res.Providers, "configured providers")
	c.Eq("openrouter", res.Default, "default provider")

	c.Eq("openrouter", f.parse().DefaultProvider, "default_provider in the file")
	c.NotStrContains(string(f.fileBytes()), "sk-good", "the key must never reach providers.toml:\n")
	c.StrContains(f.serviceEnv(), openrouterAPIKeyEnv, "the key must reach service.env:\n")
	c.Eq(sha256Hex(f.fileBytes()), f.st.Providers.FileSHA256, "the recorded hash is the file's")
}

func TestProvidersStepBothKeysAnthropicDefaultWithFallback(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	c.NoError(f.cmd.Flags().Set(keyOpenRouterFlag, "sk-or"), "set openrouter flag")
	c.NoError(f.cmd.Flags().Set(keyAnthropicFlag, "sk-ant"), "set anthropic flag")

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.Eq("anthropic", res.Default, "default provider")

	set := f.parse()
	c.Eq("anthropic", set.DefaultProvider, "default_provider")
	c.EqDeep([]string{"openrouter"}, set.Providers["anthropic"].Fallback, "anthropic fallback")
}

func TestProvidersStepSecondRunIsNoop(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	c.NoError(f.cmd.Flags().Set(keyOpenRouterFlag, "sk-good"), "set key flag")

	savedEnv := map[string]string{}
	if _, err := f.run(savedEnv); err != nil {
		t.Fatalf("first run: %v", err)
	}
	info1, err := os.Stat(f.path)
	c.NoError(err, "stat after first run")
	savedEnv[openrouterAPIKeyEnv] = "sk-good"

	res, err := f.run(savedEnv)
	c.NoError(err, "second run")
	c.False(res.Changed, "an unchanged re-run must report Changed=false")

	info2, err := os.Stat(f.path)
	c.NoError(err, "stat after second run")
	c.Eq(info1.ModTime(), info2.ModTime(), "an unchanged re-run must not rewrite the file")
}

func TestProvidersStepForeignFileKeptUnderNonInteractive(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	hand := "default_provider = \"openrouter\"\n\n[providers.openrouter]\nkind = \"anthropic-openrouter\"\napi_key_env = \"OPENROUTER_API_KEY\"\n"
	c.NoError(os.WriteFile(f.path, []byte(hand), 0o644), "write hand-edited file")

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.EqDeep([]string{"openrouter"}, res.Providers, "providers from the kept file")
	c.Eq("openrouter", res.Default, "default from the kept file")
	c.Eq(hand, string(f.fileBytes()), "a kept file must be left byte-identical")
	c.Empty(f.backups(), "keep must not create a backup")
	c.Eq("", f.st.Providers.FileSHA256, "keep must not record a hash")
	c.StrContains(f.out.String(), "openrouter: anthropic-openrouter", "a per-provider summary line is missing:\n")
}

func TestProvidersStepForeignInvalidTOMLFails(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	c.NoError(os.WriteFile(f.path, []byte("[unclosed\n"), 0o644), "write garbage")

	_, err := f.run(nil)
	c.Require().Error(err, "an unparseable kept file must be an error")
	c.StrContains(err.Error(), f.path, "the error must name the file:\n")
}

func TestProvidersStepOwnedKeepsSummariesAndEmbeddings(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	owned := &providers.Set{
		DefaultProvider: "openrouter",
		Providers: map[string]providers.Provider{
			"openrouter": {Kind: providers.KindAnthropicOpenRouter, APIKeyEnv: openrouterAPIKeyEnv},
		},
		Summaries:  &providers.SummariesConfig{Model: "openrouter/z-ai/glm-5.3-flash", MaxSegmentTokens: 1234},
		Embeddings: &providers.EmbeddingsConfig{URL: openrouterEmbeddingsURL, APIKeyEnv: openrouterAPIKeyEnv, Model: openrouterSuggestedEmbedModel, Dimensions: 1536},
	}
	b, err := renderProviders(owned)
	c.NoError(err, "render owned file")
	c.NoError(os.WriteFile(f.path, b, 0o644), "write owned file")
	f.st.Providers.FileSHA256 = sha256Hex(b)

	_, err = f.run(map[string]string{openrouterAPIKeyEnv: "sk-x"})
	c.NoError(err, "providers step")

	c.Eq(string(b), string(f.fileBytes()), "an owned file with unchanged answers must be byte-identical")
	set := f.parse()
	c.Require().NotNil(set.Summaries, "summaries preserved")
	c.Eq("openrouter/z-ai/glm-5.3-flash", set.Summaries.Model, "summaries model")
	c.Eq(1234, set.Summaries.MaxSegmentTokens, "summaries max_segment_tokens preserved")
	c.Require().NotNil(set.Embeddings, "embeddings preserved")
	c.Eq(openrouterSuggestedEmbedModel, set.Embeddings.Model, "embeddings model")
}

func TestProvidersStepNoProvidersWritesNoFile(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.Empty(res.Providers, "no providers")
	c.False(res.Changed, "nothing changed")
	c.StrContains(f.out.String(), noProviderNote, "the no-provider note is missing:\n")
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Errorf("no providers must write no file, stat err = %v", err)
	}
}

func TestProvidersStepOwnedFileMovedWhenEmptied(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	f.r.paths["ollama"] = "/usr/bin/ollama"
	owned := &providers.Set{
		DefaultProvider: "ollama",
		Providers: map[string]providers.Provider{
			"ollama": {Kind: providers.KindOpenAI, BaseURL: "http://localhost:11434/v1"},
		},
	}
	b, err := renderProviders(owned)
	c.NoError(err, "render owned file")
	c.NoError(os.WriteFile(f.path, b, 0o644), "write owned file")
	f.st.Providers.FileSHA256 = sha256Hex(b)
	f.st.Optional = []string{"ollama"}

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.True(res.Changed, "moving the file is a change")
	c.Empty(res.Providers, "the only provider was dropped")
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Errorf("the emptied owned file must be moved away, stat err = %v", err)
	}
	backups := f.backups()
	c.Require().Len(backups, 1, "exactly one backup")
	c.Eq(string(b), string(mustRead(t, backups[0])), "the backup holds the old bytes")
	c.Eq("", f.st.Providers.FileSHA256, "the hash is cleared")
}

func TestProvidersStepOllamaAnswering(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	f.st.Optional = []string{"ollama"}
	f.r.paths["ollama"] = "/usr/bin/ollama"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	ollamaBaseURL = srv.URL

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.EqDeep([]string{"ollama"}, res.Providers, "ollama configured")

	p, ok := f.parse().Providers["ollama"]
	c.True(ok, "ollama provider present")
	c.Eq(providers.KindOpenAI, p.Kind, "ollama kind")
	c.Eq(srv.URL+"/v1", p.BaseURL, "ollama base URL")
	c.Empty(f.r.interCalls, "non-interactive must not pull a model")
}

func TestProvidersStepOllamaNotAnsweringSkipped(t *testing.T) {
	c := assert.NewAborting(t)
	f := newProvidersFixture(t)
	f.st.Optional = []string{"ollama"}
	f.r.paths["ollama"] = "/usr/bin/ollama"

	res, err := f.run(nil)
	c.NoError(err, "providers step")
	c.Empty(res.Providers, "a silent ollama is dropped")
	c.StrContains(f.out.String(), "ollama: not answering; skipped", "the skip line is missing:\n")
	if _, ok := f.parseSafe(); ok {
		t.Error("no provider should have been written")
	}
}

// parseSafe parses the file when it exists.
func (f *providersFixture) parseSafe() (*providers.Set, bool) {
	f.t.Helper()
	if _, err := os.Stat(f.path); os.IsNotExist(err) {
		return nil, false
	}
	set, err := providers.Parse(f.fileBytes())
	if err != nil {
		return nil, false
	}
	return set, true
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestProvidersStepOwnedKeepsSummariesModel(t *testing.T) {
	// A non-interactive re-run must not drop a [summaries] model setup already
	// wrote, even one not in the offered suggestions: a custom provider/model
	// id, or ollama/<m> whose chat model is not re-chosen non-interactively.
	for _, model := range []string{"my-custom/model", "ollama/qwen3.5:9b"} {
		t.Run(model, func(t *testing.T) {
			c := assert.NewAborting(t)
			f := newProvidersFixture(t)
			owned := &providers.Set{
				DefaultProvider: "openrouter",
				Providers: map[string]providers.Provider{
					"openrouter": {Kind: providers.KindAnthropicOpenRouter, APIKeyEnv: openrouterAPIKeyEnv},
				},
				Summaries: &providers.SummariesConfig{Model: model, MaxSegmentTokens: 4321},
			}
			b, err := renderProviders(owned)
			c.NoError(err, "render owned file")
			c.NoError(os.WriteFile(f.path, b, 0o644), "write owned file")
			f.st.Providers.FileSHA256 = sha256Hex(b)

			_, err = f.run(map[string]string{openrouterAPIKeyEnv: "sk-x"})
			c.NoError(err, "providers step")

			c.Eq(string(b), string(f.fileBytes()), "an owned summaries model must survive a non-interactive re-run byte-identically")
			set := f.parse()
			c.Require().NotNil(set.Summaries, "summaries preserved")
			c.Eq(model, set.Summaries.Model, "summaries model")
			c.Eq(4321, set.Summaries.MaxSegmentTokens, "max_segment_tokens preserved")
		})
	}
}

// TestSetupProvidersOffersResponsesKinds pins the two Responses entries the
// providers step now offers: what gets written for each, that the api-key kind's
// key goes to service.env rather than providers.toml, and that a re-run over a
// file setup already wrote keeps both (they are builtin menu names, so a re-run
// must not re-offer them as custom OpenAI-compatible endpoints).
func TestSetupProvidersOffersResponsesKinds(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(containsString(providerMenu, responsesProviderName), "the menu must offer %q: %v", responsesProviderName, providerMenu)
	c.True(containsString(providerMenu, chatgptProviderName), "the menu must offer %q: %v", chatgptProviderName, providerMenu)
	for _, name := range []string{responsesProviderName, chatgptProviderName} {
		c.True(isBuiltinProvider(name), "%q must be a builtin menu name, else a re-run treats it as a custom endpoint", name)
	}

	t.Run("api-key kind from a saved key", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newProvidersFixture(t)

		// A key in service.env preselects the provider non-interactively, and an
		// unchanged key is never re-validated (no network on a re-run).
		res, err := f.run(map[string]string{openaiAPIKeyEnv: "sk-openai"})
		c.NoError(err, "providers step")
		c.EqDeep([]string{responsesProviderName}, res.Providers, "configured providers")
		c.Eq(responsesProviderName, res.Default, "the only provider is the default")

		set := f.parse()
		p, ok := set.Providers[responsesProviderName]
		c.True(ok, "providers.toml must carry %q:\n%s", responsesProviderName, f.fileBytes())
		c.Eq(providers.KindOpenAIResponses, p.Kind, "kind")
		c.Eq(responsesDefaultBaseURL, p.BaseURL, "base_url default")
		c.Eq(openaiAPIKeyEnv, p.APIKeyEnv, "api_key_env default")
		c.NotStrContains(string(f.fileBytes()), "sk-openai", "the key must never reach providers.toml:\n")
		c.StrContains(f.serviceEnv(), openaiAPIKeyEnv, "the key must reach service.env:\n")
		c.NotStrContains(f.out.String(), "rafiki providers login", "only the subscription kind needs a login:\n")
	})

	t.Run("subscription kind survives a re-run", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newProvidersFixture(t)
		owned := &providers.Set{
			DefaultProvider: responsesProviderName,
			Providers: map[string]providers.Provider{
				responsesProviderName: {Kind: providers.KindOpenAIResponses, BaseURL: "https://api.openai.com/v1", APIKeyEnv: openaiAPIKeyEnv},
				chatgptProviderName:   {Kind: providers.KindOpenAIResponsesChatGPT},
			},
		}
		b, err := renderProviders(owned)
		c.NoError(err, "render owned file")
		c.NoError(os.WriteFile(f.path, b, 0o644), "write owned file")
		f.st.Providers.FileSHA256 = sha256Hex(b)
		// The key is already in service.env, so a re-run has nothing to change.
		_, err = paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{openaiAPIKeyEnv: "sk-openai"}, "test fixture")
		c.NoError(err, "seed service.env")

		res, err := f.run(map[string]string{openaiAPIKeyEnv: "sk-openai"})
		c.NoError(err, "providers step")
		c.False(res.Changed, "an unchanged re-run must report Changed=false")
		c.Eq(string(b), string(f.fileBytes()), "the owned file must survive byte-identically")

		set := f.parse()
		c.Eq(providers.KindOpenAIResponsesChatGPT, set.Providers[chatgptProviderName].Kind, "the subscription kind must be kept")
		c.Eq("", set.Providers[chatgptProviderName].APIKeyEnv, "the subscription kind carries no api_key_env")
		c.Eq("", set.Providers[chatgptProviderName].BaseURL, "the subscription kind carries no base_url")
		c.StrContains(f.out.String(), "each user runs: rafiki providers login "+chatgptProviderName,
			"the subscription kind must say how to get a credential:\n")
	})
}

// TestSetupValidateResponsesKey pins the api-key kind's live check: the endpoint
// setup is about to write answers GET /models, and only a 200 validates.
func TestSetupValidateResponsesKey(t *testing.T) {
	c := assert.NewAborting(t)
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c.NoError(validateResponsesKey(context.Background(), srv.URL+"/", "sk-openai"), "a 200 must validate")
	c.Eq("/models", path, "the check is GET <base_url>/models")
	c.Eq("Bearer sk-openai", auth, "the key rides a bearer header")

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(fail.Close)
	c.Error(validateResponsesKey(context.Background(), fail.URL, "sk-bad"), "a non-200 must not validate")
}
