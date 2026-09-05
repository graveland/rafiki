// SPDX-License-Identifier: Apache-2.0

package proxyenv

import (
	"fmt"
	"strings"
)

// PassthroughMode is the parsed form of a --passthrough-auth flag or
// RAFIKI_CLAUDE_PASSTHROUGH/similar env var. A tri-state rather than a bool
// because "unset" and "off" are genuinely different requests: unset means let
// the model decide, off means bill the daemon's key no matter what the model
// is. Mirrors the RTKMode (auto/on/off) convention already used for --rtk and
// --bash-rtk.
//
// Shared by `rafiki claude` (an interactive session) and rafikid's daraja
// launch path (a daemon-managed one) so the two auth decisions cannot drift —
// see this package's own doc comment for why that drift is the whole reason
// this package exists.
type PassthroughMode string

const (
	// PassthroughAuto bills your own subscription when the model resolves to
	// an Anthropic id (including no model override at all) and the daemon's
	// key otherwise.
	PassthroughAuto PassthroughMode = "auto"
	// PassthroughOn forces the subscription and rejects a non-Anthropic model.
	PassthroughOn PassthroughMode = "on"
	// PassthroughOff always bills the daemon's key.
	PassthroughOff PassthroughMode = "off"
)

// ParsePassthroughMode parses a --passthrough-auth flag or
// RAFIKI_CLAUDE_PASSTHROUGH-shaped value. true/1 and false/0 remain accepted
// aliases for on/off, matching the flag's old boolean form. Unlike RTKMode's
// ParseRTKMode, an unrecognised value is a hard error rather than a silent
// fallback to auto: this switch decides who gets billed, and a typo must not
// decide that quietly.
func ParsePassthroughMode(s string) (PassthroughMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return PassthroughAuto, nil
	case "on", "true", "1":
		return PassthroughOn, nil
	case "off", "false", "0", "no":
		return PassthroughOff, nil
	default:
		return "", fmt.Errorf("invalid passthrough-auth value %q: want auto, on, or off", s)
	}
}

// PassthroughAuthFor resolves a parsed PassthroughMode against model into the
// bool ClaudeOptions.PassthroughAuth wants.
func PassthroughAuthFor(mode PassthroughMode, model string) bool {
	switch mode {
	case PassthroughOn:
		return true
	case PassthroughOff:
		return false
	default: // PassthroughAuto
		return AnthropicModel(model)
	}
}
