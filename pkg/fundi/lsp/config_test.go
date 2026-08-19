package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"go":{"command":"gopls"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, ok := cfg.Servers["go"]; !ok {
		t.Errorf("Servers = %v, want a \"go\" entry", cfg.Servers)
	}
}

// A config with no servers key must still yield a non-nil map: every caller
// ranges over Servers, and a nil map that is later assigned into would panic.
func TestLoadConfigAlwaysReturnsANonNilServerMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Servers == nil {
		t.Error("Servers is nil; want an empty non-nil map")
	}
}

func TestLoadConfigRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("LoadConfig accepted invalid JSON")
	}
}
