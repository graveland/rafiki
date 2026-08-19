package lsp

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadConfig reads an lsp.json into a Config.
//
// It lives beside the Config type rather than in the caller because there are
// now two callers on opposite sides of a dependency boundary: the agent runtime
// in pkg/fundi, and the executor, which must not import pkg/fundi because that
// package links pgx and pkg/executor is required to link none.
//
// Servers is always non-nil on success. Every caller ranges over it, and one
// of them assigns into it.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("lsp: invalid config %s: %w", path, err)
	}
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]ServerConfig)
	}
	return cfg, nil
}
