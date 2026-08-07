// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the daemon's optional deployment config file. Everything in it has
// an environment-variable or flag equivalent for local use; the file exists for
// the one thing environment variables cannot express — a MAP of named client
// tokens, where each name becomes the captured owner identity.
type Config struct {
	Tokens       map[string]string `yaml:"tokens"`
	OpenAIRoutes []OpenAIRoute     `yaml:"openai_routes"`
	DefaultModel string            `yaml:"default_model"`
}

// OpenAIRoute maps a model-id prefix to an upstream name on the
// /v1/chat/completions face ("openrouter" is the built-in default).
type OpenAIRoute struct {
	Prefix   string `yaml:"prefix"`
	Upstream string `yaml:"upstream"`
}

// loadConfig reads path, or returns a zero Config when path is empty. A named
// file that cannot be read or parsed is an error: the operator asked for it, so
// silently falling back to defaults would serve the wrong credentials.
func loadConfig(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}
