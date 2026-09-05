// SPDX-License-Identifier: Apache-2.0

package proxyenv

import "testing"

func TestParsePassthroughMode(t *testing.T) {
	cases := []struct {
		in   string
		want PassthroughMode
	}{
		{"", PassthroughAuto},
		{"auto", PassthroughAuto},
		{"AUTO", PassthroughAuto},
		{"on", PassthroughOn},
		{"true", PassthroughOn},
		{"1", PassthroughOn},
		{"off", PassthroughOff},
		{"false", PassthroughOff},
		{"0", PassthroughOff},
		{"no", PassthroughOff},
	}
	for _, tc := range cases {
		got, err := ParsePassthroughMode(tc.in)
		if err != nil {
			t.Fatalf("ParsePassthroughMode(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParsePassthroughMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParsePassthroughModeRejectsGarbage(t *testing.T) {
	if _, err := ParsePassthroughMode("onn"); err == nil {
		t.Fatal("want an error for an unrecognised value")
	}
}

func TestPassthroughAuthFor(t *testing.T) {
	if !PassthroughAuthFor(PassthroughAuto, "") {
		t.Error("auto + no model should bill the subscription (Claude Code picks its own Anthropic id)")
	}
	if !PassthroughAuthFor(PassthroughAuto, "claude-opus-5") {
		t.Error("auto + anthropic model should bill the subscription")
	}
	if PassthroughAuthFor(PassthroughAuto, "openai/gpt-4o") {
		t.Error("auto + non-anthropic model should bill the daemon's key")
	}
	if !PassthroughAuthFor(PassthroughOn, "openai/gpt-4o") {
		t.Error("on must force passthrough regardless of model")
	}
	if PassthroughAuthFor(PassthroughOff, "claude-opus-5") {
		t.Error("off must force the daemon's key regardless of model")
	}
}
