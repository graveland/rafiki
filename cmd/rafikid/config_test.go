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
	if len(cfg.Tokens) != 0 || len(cfg.OpenAIRoutes) != 0 || cfg.DefaultModel != "" {
		t.Errorf("empty path should yield a zero Config, got %+v", cfg)
	}
}

func TestLoadConfig_ParsesTokensRoutesAndModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rafiki.yaml")
	body := `tokens:
  sentinel: tok-sentinel
  editor: tok-editor
openai_routes:
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
	if got := cfg.Tokens["sentinel"]; got != "tok-sentinel" {
		t.Errorf("Tokens[sentinel] = %q, want tok-sentinel", got)
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
	body := "tokens: [this is not valid yaml"
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

// The daemon's flags must not swallow a subcommand. `rafikid agent --model x`
// is dispatched before parsing; parseDaemonFlags only ever sees daemon args.
func TestParseDaemonFlags(t *testing.T) {
	f, err := parseDaemonFlags([]string{"--dev", "--listen", "127.0.0.1:9000", "--config", "/tmp/c.yaml"})
	if err != nil {
		t.Fatalf("parseDaemonFlags: %v", err)
	}
	if !f.Dev || f.Listen != "127.0.0.1:9000" || f.Config != "/tmp/c.yaml" {
		t.Errorf("parsed %+v, want dev=true listen=127.0.0.1:9000 config=/tmp/c.yaml", f)
	}
}

func TestParseDaemonFlags_NoArgs(t *testing.T) {
	f, err := parseDaemonFlags(nil)
	if err != nil {
		t.Fatalf("parseDaemonFlags(nil): %v", err)
	}
	if f.Dev || f.Listen != "" || f.Config != "" || f.DB != "" {
		t.Errorf("no args should yield a zero daemonFlags, got %+v", f)
	}
}
