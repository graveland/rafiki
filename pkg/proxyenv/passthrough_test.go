// SPDX-License-Identifier: Apache-2.0

package proxyenv

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestParsePassthroughMode(t *testing.T) {
	c := assert.NewCollecting(t)
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
		c.Require().NoError(err, "ParsePassthroughMode(%q)", tc.in)
		c.Eq(tc.want, got, "ParsePassthroughMode(%q) = %q, want", tc.in, got)
	}
}

func TestParsePassthroughModeRejectsGarbage(t *testing.T) {
	_, err := ParsePassthroughMode("onn")
	assert.NewAborting(t).Error(err, "want an error for an unrecognised value")
}

func TestPassthroughAuthFor(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True(PassthroughAuthFor(PassthroughAuto, ""), "auto + no model should bill the subscription (Claude Code picks its own Anthropic id)")
	c.True(PassthroughAuthFor(PassthroughAuto, "claude-opus-5"), "auto + anthropic model should bill the subscription")
	c.False(PassthroughAuthFor(PassthroughAuto, "openai/gpt-4o"), "auto + non-anthropic model should bill the daemon's key")
	c.True(PassthroughAuthFor(PassthroughOn, "openai/gpt-4o"), "on must force passthrough regardless of model")
	c.False(PassthroughAuthFor(PassthroughOff, "claude-opus-5"), "off must force the daemon's key regardless of model")
}
