package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestLoadConfig_EmptyPathIsZeroValue(t *testing.T) {
	c := assert.NewCollecting(t)
	cfg, err := loadConfig("")
	c.Require().NoError(err, "loadConfig(\"\")")
	c.False(len(cfg.OpenAIRoutes) != 0 || cfg.DefaultModel != "", "empty path should yield a zero Config, got %+v", cfg)
}

func TestLoadConfig_ParsesRoutesAndModel(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rafiki.yaml")
	body := `openai_routes:
  - prefix: "moonshotai/"
    upstream: openrouter
default_model: haiku-latest
`
	c.Require().NoError(os.WriteFile(path, []byte(body), 0o600))

	cfg, err := loadConfig(path)
	c.Require().NoError(err, "loadConfig")
	c.False(len(cfg.OpenAIRoutes) != 1 || cfg.OpenAIRoutes[0].Prefix != "moonshotai/", "OpenAIRoutes = %+v, want one moonshotai/ route", cfg.OpenAIRoutes)
	c.Eq("haiku-latest", cfg.DefaultModel, "DefaultModel")
}

func TestLoadConfig_MissingFileIsAnError(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	assert.NewCollecting(t).Error(err, "loadConfig on a missing file should error, not silently yield defaults")
}

// A named config that cannot be parsed is fatal, not a silent fallback to
// defaults: the operator named a file and got one back that doesn't parse,
// which is exactly the case where guessing at defaults would serve the wrong
// credentials.
func TestLoadConfig_MalformedYAMLIsAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	// Unclosed flow mapping: not valid YAML at any indentation.
	body := "openai_routes: [this is not valid yaml"
	c.Require().NoError(os.WriteFile(path, []byte(body), 0o600))
	_, err := loadConfig(path)
	c.Error(err, "loadConfig on malformed YAML should error, not silently yield defaults")
}

// default_model precedence: the config file wins when set; an empty config
// value falls through to the environment variable.
func TestResolveDefaultModel_ConfigWinsOverEnv(t *testing.T) {
	t.Setenv("RAFIKI_DEFAULT_MODEL", "env-model")
	got := resolveDefaultModel(Config{DefaultModel: "config-model"})
	assert.NewCollecting(t).Eq("config-model", got, "resolveDefaultModel")
}

func TestResolveDefaultModel_FallsThroughToEnv(t *testing.T) {
	t.Setenv("RAFIKI_DEFAULT_MODEL", "env-model")
	got := resolveDefaultModel(Config{})
	assert.NewCollecting(t).Eq("env-model", got, "resolveDefaultModel")
}

// TestProviderGuardEnabled proves RAFIKI_PROVIDER_GUARD only disables the guard
// on an explicit off-switch. Unset, and any value that isn't a recognised
// negation, must leave it running — a budget guard that silently turns itself
// off on a typo is worse than none, because you would believe you had one.
func TestProviderGuardEnabled(t *testing.T) {
	for in, want := range map[string]bool{
		"":        true,
		"on":      true,
		"1":       true,
		"true":    true,
		"garbage": true,
		"off":     false,
		"OFF":     false,
		" off ":   false,
		"false":   false,
		"0":       false,
		"no":      false,
	} {
		got := providerGuardEnabled(in)
		assert.NewCollecting(t).Eq(want, got, "providerGuardEnabled(%q) = %v, want", in, got)
	}
}
