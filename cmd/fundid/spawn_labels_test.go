package main

import "testing"

func TestSpawnKindLabel(t *testing.T) {
	cases := map[string]string{
		"":       "pi", // empty kind defaults to pi (the implicit default)
		"pi":     "pi",
		"claude": "claude",
	}
	for in, want := range cases {
		if got := spawnKindLabel(in); got != want {
			t.Errorf("spawnKindLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
