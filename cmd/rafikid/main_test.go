// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// TestMain isolates the config directory for the whole package.
//
// resolveUpstream loads paths.ProvidersFile(), which resolves under
// XDG_CONFIG_HOME — so without this the tests read the DEVELOPER'S real
// ~/.config/rafiki/providers.toml, and whatever default_provider it names
// decides where an analyze run's request actually goes. That is not
// hypothetical: a providers.toml setting default_provider = "ollama" routed
// TestResolveUpstreamProxyDoesNotLeakAPIKey's send to localhost:11434 instead
// of its own httptest server, so the test reported "proxy server saw 0
// requests" and the two AgentAnalyze tests failed with every model unservable.
// All three pass on a machine with no providers.toml, which is exactly what
// makes the leak easy to miss.
//
// Package-wide, because the coupling is inside resolveUpstream rather than in
// any one test. The handful of tests that set XDG_CONFIG_HOME themselves still
// win: t.Setenv applies after this.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rafikid-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestParseControlListenAddr covers every spelling of RAFIKI_CONTROL_LISTEN
// documented in .env.example, README.md, and
// docs/reference/control-protocol.md, plus the already-working host:port
// forms and the failure cases.
func TestParseControlListenAddr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"documented tcp:port form", "tcp:8036", ":8036"},
		{"bare port, no prefix", "8036", ":8036"},
		{"already all-interfaces", ":8036", ":8036"},
		{"host:port", "1.2.3.4:8036", "1.2.3.4:8036"},
		{"tcp: prefix over an already-colon-prefixed port", "tcp::8036", ":8036"},
		{"empty", "", ""},
		{"genuinely invalid", "1.2.3.4:8036:extra", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(paths.ControlListen, tt.in)
			got := parseControlListenAddr()
			if got != tt.want {
				t.Errorf("parseControlListenAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestExecutorsEnabled covers RAFIKI_EXECUTORS_ENABLED's default, which
// depends on whether a TCP control listener is configured: unset defaults ON
// with no control listener (the only path in is the local UDS socket, same
// trust boundary as the daemon's own control socket) and OFF once one is
// set (that path becomes reachable over the network). Explicit values always
// win, and none of them matter without a database to back the store.
func TestExecutorsEnabled(t *testing.T) {
	tests := []struct {
		name         string
		envVal       string // unset when empty and no explicit case; use hasEnv to force ""
		hasEnv       bool
		controlAddr  string
		dbConfigured bool
		want         bool
	}{
		{"unset, no control listener, db configured: defaults on", "", false, "", true, true},
		{"unset, control listener set, db configured: defaults off", "", false, ":8036", true, false},
		{"unset, no control listener, no db: off regardless", "", false, "", false, false},
		{"explicit 1, control listener set, db configured: on", "1", true, ":8036", true, true},
		{"explicit 1, no db: still off", "1", true, "", false, false},
		{"explicit 0, no control listener, db configured: off", "0", true, "", true, false},
		{"explicit false, control listener set, db configured: off", "false", true, ":8036", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hasEnv {
				t.Setenv(paths.ExecutorsEnabled, tt.envVal)
			} else {
				t.Setenv(paths.ExecutorsEnabled, "")
			}
			got := executorsEnabled(tt.controlAddr, tt.dbConfigured)
			if got != tt.want {
				t.Errorf("executorsEnabled(%q, %v) = %v, want %v", tt.controlAddr, tt.dbConfigured, got, tt.want)
			}
		})
	}
}

// TestParseLogLevel covers the four level names the --log-level help
// advertises, case-insensitivity, and the refusal of everything else —
// including slog's own offset forms ("INFO+2"), which are more power than
// selecting a daemon's verbosity needs.
func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{" Warn ", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"", slog.LevelInfo, true},
		{"verbose", slog.LevelInfo, true},
		{"INFO+2", slog.LevelInfo, true},
	}
	for _, tt := range tests {
		got, err := parseLogLevel(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("parseLogLevel(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

// TestResolveLogLevel pins the precedence chain (flag > RAFIKI_LOG_LEVEL >
// info) and the asymmetry between the two sources: an invalid flag fails the
// run, an invalid env value warns and falls back to info.
func TestResolveLogLevel(t *testing.T) {
	if level, err := resolveLogLevel("debug"); err != nil || level != slog.LevelDebug {
		t.Errorf("flag debug = %v, %v; want debug, nil", level, err)
	}
	if _, err := resolveLogLevel("louder"); err == nil {
		t.Error("invalid flag must fail the run, got nil error")
	}

	t.Setenv(paths.LogLevel, "warn")
	if level, err := resolveLogLevel(""); err != nil || level != slog.LevelWarn {
		t.Errorf("env warn = %v, %v; want warn, nil", level, err)
	}
	if level, err := resolveLogLevel("error"); err != nil || level != slog.LevelError {
		t.Errorf("flag must beat env = %v, %v; want error, nil", level, err)
	}

	t.Setenv(paths.LogLevel, "nonsense")
	level, err := resolveLogLevel("")
	if err != nil || level != slog.LevelInfo {
		t.Errorf("invalid env = %v, %v; want info fallback, nil error", level, err)
	}

	t.Setenv(paths.LogLevel, "")
	if level, err := resolveLogLevel(""); err != nil || level != slog.LevelInfo {
		t.Errorf("unset everywhere = %v, %v; want info, nil", level, err)
	}
}
