package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestSpawnKindLabel(t *testing.T) {
	cases := map[string]string{
		"":                  protocol.KindPi, // empty kind defaults to pi (the implicit default)
		protocol.KindPi:     protocol.KindPi,
		protocol.KindClaude: protocol.KindClaude,
		protocol.KindFundi:  protocol.KindFundi,
	}
	for in, want := range cases {
		if got := spawnKindLabel(in); got != want {
			t.Errorf("spawnKindLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
