// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
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

// daemonFlags is the daemon's own flag set. It had none before the standalone
// `rafiki serve` was folded in.
type daemonFlags struct {
	Config string
	Listen string
	DB     string
	Dev    bool
}

// parseDaemonFlags parses the daemon's arguments. Subcommands (agent, migrate)
// are dispatched by main() BEFORE this runs, so args here are only ever the
// daemon's own.
func parseDaemonFlags(args []string) (daemonFlags, error) {
	var f daemonFlags
	fs := flag.NewFlagSet("rafikid", flag.ContinueOnError)
	fs.StringVar(&f.Config, "config", "", "config file (named client tokens, openai routes, default model)")
	fs.StringVar(&f.Listen, "listen", "", "proxy face listen address (overrides RAFIKI_PROXY_LISTEN)")
	fs.StringVar(&f.DB, "db", "", "postgres DSN (overrides RAFIKI_DB)")
	fs.BoolVar(&f.Dev, "dev", false, "dev mode: auto-migrate the schema, accept the token \"dev\"")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	return f, nil
}
