package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_EmptyPathIsZeroValue(t *testing.T) {
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("loadConfig(\"\"): %v", err)
	}
	if len(cfg.OpenAIRoutes) != 0 || cfg.DefaultModel != "" {
		t.Errorf("empty path should yield a zero Config, got %+v", cfg)
	}
}

func TestLoadConfig_ParsesRoutesAndModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rafiki.yaml")
	body := `openai_routes:
  - prefix: "moonshotai/"
    upstream: openrouter
default_model: haiku-latest
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.OpenAIRoutes) != 1 || cfg.OpenAIRoutes[0].Prefix != "moonshotai/" {
		t.Errorf("OpenAIRoutes = %+v, want one moonshotai/ route", cfg.OpenAIRoutes)
	}
	if cfg.DefaultModel != "haiku-latest" {
		t.Errorf("DefaultModel = %q, want haiku-latest", cfg.DefaultModel)
	}
}

func TestLoadConfig_MissingFileIsAnError(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("loadConfig on a missing file should error, not silently yield defaults")
	}
}

// A named config that cannot be parsed is fatal, not a silent fallback to
// defaults: the operator named a file and got one back that doesn't parse,
// which is exactly the case where guessing at defaults would serve the wrong
// credentials.
func TestLoadConfig_MalformedYAMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	// Unclosed flow mapping: not valid YAML at any indentation.
	body := "openai_routes: [this is not valid yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Error("loadConfig on malformed YAML should error, not silently yield defaults")
	}
}

// default_model precedence: the config file wins when set; an empty config
// value falls through to the environment variable.
func TestResolveDefaultModel_ConfigWinsOverEnv(t *testing.T) {
	t.Setenv("RAFIKI_DEFAULT_MODEL", "env-model")
	got := resolveDefaultModel(Config{DefaultModel: "config-model"})
	if got != "config-model" {
		t.Errorf("resolveDefaultModel = %q, want config-model (config should win over env)", got)
	}
}

func TestResolveDefaultModel_FallsThroughToEnv(t *testing.T) {
	t.Setenv("RAFIKI_DEFAULT_MODEL", "env-model")
	got := resolveDefaultModel(Config{})
	if got != "env-model" {
		t.Errorf("resolveDefaultModel = %q, want env-model (empty config should fall through to env)", got)
	}
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
		if got := providerGuardEnabled(in); got != want {
			t.Errorf("providerGuardEnabled(%q) = %v, want %v", in, got, want)
		}
	}
}
