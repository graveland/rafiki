package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestSpawnKindLabel(t *testing.T) {
	cases := map[string]string{
		"":                  protocol.KindFundi, // empty kind defaults to fundi
		protocol.KindClaude: protocol.KindClaude,
		protocol.KindFundi:  protocol.KindFundi,
	}
	for in, want := range cases {
		got := spawnKindLabel(in)
		assert.NewCollecting(t).Eq(want, got, "spawnKindLabel(%q) = %q, want", in, got)
	}
}
