package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestLoadConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	c.Require().NoError(os.WriteFile(path, []byte(`{"servers":{"go":{"command":"gopls"}}}`), 0o600))

	cfg, err := LoadConfig(path)
	c.Require().NoError(err, "LoadConfig")
	_, ok := cfg.Servers["go"]
	c.True(ok, "Servers = %v, want a \"go\" entry", cfg.Servers)
}

// A config with no servers key must still yield a non-nil map: every caller
// ranges over Servers, and a nil map that is later assigned into would panic.
func TestLoadConfigAlwaysReturnsANonNilServerMap(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	c.Require().NoError(os.WriteFile(path, []byte(`{}`), 0o600))

	cfg, err := LoadConfig(path)
	c.Require().NoError(err, "LoadConfig")
	c.NotNil(cfg.Servers, "Servers is nil; want an empty non-nil map")
}

func TestLoadConfigRejectsInvalidJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "lsp.json")
	c.Require().NoError(os.WriteFile(path, []byte(`{not json`), 0o600))
	_, err := LoadConfig(path)
	c.Error(err, "LoadConfig accepted invalid JSON")
}
