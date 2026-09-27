package protocol

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestKindConstants(t *testing.T) {
	cases := map[string]string{
		KindFundi:  "fundi",
		KindClaude: "claude",
	}
	for got, want := range cases {
		assert.NewCollecting(t).Eq(want, got, "kind constant")
	}
}
