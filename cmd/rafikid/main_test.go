package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

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
