// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// providerProjection is what renderProviders promises to round-trip: the
// fields setup writes, with Parse's stamped Name projected away.
type providerProjection struct {
	Kind      providers.Kind
	BaseURL   string
	APIKeyEnv string
	Fallback  []string
}

func projectProviders(set *providers.Set) map[string]providerProjection {
	out := make(map[string]providerProjection, len(set.Providers))
	for name, p := range set.Providers {
		out[name] = providerProjection{Kind: p.Kind, BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Fallback: p.Fallback}
	}
	return out
}

func sampleProvidersSet() *providers.Set {
	return &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic": {
				Kind:      providers.KindAnthropic,
				APIKeyEnv: "ANTHROPIC_API_KEY",
				Fallback:  []string{"openrouter"},
			},
			"openrouter": {
				Kind:      providers.KindAnthropicOpenRouter,
				APIKeyEnv: "OPENROUTER_API_KEY",
			},
			"ollama": {
				Kind:    providers.KindOpenAI,
				BaseURL: "http://localhost:11434/v1",
			},
		},
		Embeddings: &providers.EmbeddingsConfig{
			URL:        "http://localhost:11434/v1/embeddings",
			Model:      "nomic-embed-text:v1.5",
			Dimensions: 768,
		},
		Summaries: &providers.SummariesConfig{
			Model:            "anthropic/claude-haiku-4-5",
			MaxSegmentTokens: 6000,
		},
	}
}

func TestRenderProvidersRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)

	set := sampleProvidersSet()
	b, err := renderProviders(set)
	c.Require().NoError(err, "renderProviders")

	c.True(bytes.HasPrefix(b, []byte(providersHeader)), "output must start with providersHeader")
	c.False(bytes.Contains(b, []byte(`= ""`)), "output must not contain an empty-string assignment:\n%s", b)

	parsed, err := providers.Parse(b)
	c.Require().NoError(err, "providers.Parse of rendered bytes")

	c.Eq(set.DefaultProvider, parsed.DefaultProvider, "default_provider")
	c.EqDeep(projectProviders(set), projectProviders(parsed), "providers projection")
	c.EqDeep(set.Embeddings, parsed.Embeddings, "embeddings")
	c.EqDeep(set.Summaries, parsed.Summaries, "summaries")
}

func TestRenderProvidersDeterministic(t *testing.T) {
	c := assert.NewCollecting(t)

	set := sampleProvidersSet()
	first, err := renderProviders(set)
	c.Require().NoError(err, "renderProviders")
	for i := 0; i < 20; i++ {
		b, err := renderProviders(set)
		c.Require().NoError(err, "renderProviders #%d", i)
		if !bytes.Equal(first, b) {
			t.Fatalf("render #%d differs from the first:\nfirst:\n%s\ngot:\n%s", i, first, b)
		}
	}
}

func TestRenderProvidersRefusesUnwrittenFields(t *testing.T) {
	base := func() *providers.Set {
		return &providers.Set{
			DefaultProvider: "anthropic",
			Providers: map[string]providers.Provider{
				"anthropic": {Kind: providers.KindAnthropic, APIKeyEnv: "ANTHROPIC_API_KEY"},
			},
		}
	}
	cases := []struct {
		name   string
		mutate func(*providers.Set)
	}{
		{"models", func(s *providers.Set) {
			p := s.Providers["anthropic"]
			p.Models = map[string]providers.ModelAlias{"qwen": {ID: "m"}}
			s.Providers["anthropic"] = p
		}},
		{"extras", func(s *providers.Set) {
			p := s.Providers["anthropic"]
			p.Extras = map[string]any{"foo": "bar"}
			s.Providers["anthropic"] = p
		}},
		{"via_executor", func(s *providers.Set) {
			p := s.Providers["anthropic"]
			p.BaseURL = "http://localhost:9000"
			p.ViaExecutor = &providers.ViaExecutor{Selector: "sel", Proxy: "http://localhost:8080"}
			s.Providers["anthropic"] = p
		}},
		{"session_header", func(s *providers.Set) {
			p := s.Providers["anthropic"]
			p.SessionHeader = "x-session-id"
			s.Providers["anthropic"] = p
		}},
		{"invalid", func(s *providers.Set) {
			s.DefaultProvider = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			set := base()
			tc.mutate(set)
			_, err := renderProviders(set)
			c.Error(err, "renderProviders must refuse a %s field", tc.name)
		})
	}
}

func TestClassifyProvidersFile(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.toml")

	ownedPath := filepath.Join(dir, "owned.toml")
	ownedBytes := []byte("default_provider = \"anthropic\"\n")
	if err := os.WriteFile(ownedPath, ownedBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	foreignPath := filepath.Join(dir, "foreign.toml")
	if err := os.WriteFile(foreignPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		path        string
		recordedSHA string
		wantState   providersFileState
		wantNil     bool
	}{
		{"missing", missing, sha256Hex(ownedBytes), providersFileMissing, true},
		{"owned", ownedPath, sha256Hex(ownedBytes), providersFileOwned, false},
		{"foreign empty hash", foreignPath, "", providersFileForeign, false},
		{"foreign different hash", foreignPath, sha256Hex([]byte("different")), providersFileForeign, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			state, current, err := classifyProvidersFile(tc.path, tc.recordedSHA)
			c.Require().NoError(err, "classifyProvidersFile")
			c.Eq(int(tc.wantState), int(state), "state")
			if tc.wantNil {
				c.Nil(current, "current bytes")
			} else {
				c.NotNil(current, "current bytes")
			}
		})
	}
}

func TestBackupProvidersFile(t *testing.T) {
	c := assert.NewCollecting(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.toml")
	original := []byte("default_provider = \"anthropic\"\n")
	c.Require().NoError(os.WriteFile(path, original, 0o644), "seed providers.toml")

	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	first, err := backupProvidersFile(path, now)
	c.Require().NoError(err, "first backup")
	c.Eq(path+".20261016T120000Z.bak", first, "first backup path")
	got, err := os.ReadFile(first)
	c.Require().NoError(err, "read first backup")
	c.EqDeep(original, got, "first backup bytes")

	second, err := backupProvidersFile(path, now)
	c.Require().NoError(err, "second backup at the same instant")
	c.Eq(path+".20261016T120000Z-1.bak", second, "second backup path")

	// The first backup must be left untouched by the second.
	got, err = os.ReadFile(first)
	c.Require().NoError(err, "re-read first backup")
	c.EqDeep(original, got, "the first backup must be untouched")
}

func TestWriteProvidersFileUnchangedDoesNotWrite(t *testing.T) {
	c := assert.NewCollecting(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "providers.toml")

	b := []byte("default_provider = \"anthropic\"\n")
	changed, err := writeProvidersFile(path, b)
	c.Require().NoError(err, "first write")
	c.True(changed, "the first write must report changed")

	info, err := os.Stat(path)
	c.Require().NoError(err, "stat after first write")
	firstMtime := info.ModTime()
	c.Eq(os.FileMode(0o644), info.Mode().Perm(), "providers.toml must be 0644")

	time.Sleep(10 * time.Millisecond)

	changed, err = writeProvidersFile(path, b)
	c.Require().NoError(err, "identical write")
	c.False(changed, "an identical write must report changed == false")
	info, err = os.Stat(path)
	c.Require().NoError(err, "stat after identical write")
	c.Eq(firstMtime, info.ModTime(), "an identical write must not touch the file")

	changed, err = writeProvidersFile(path, []byte("default_provider = \"openrouter\"\n"))
	c.Require().NoError(err, "different write")
	c.True(changed, "a different write must report changed == true")
	info, err = os.Stat(path)
	c.Require().NoError(err, "stat after different write")
	c.Eq(os.FileMode(0o644), info.Mode().Perm(), "rewritten providers.toml must be 0644")
}
