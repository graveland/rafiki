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
