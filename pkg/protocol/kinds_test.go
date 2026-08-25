package protocol

import "testing"

func TestKindConstants(t *testing.T) {
	cases := map[string]string{
		KindFundi:  "fundi",
		KindClaude: "claude",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("kind constant = %q, want %q", got, want)
		}
	}
}
