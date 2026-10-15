// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"os"
	"strings"
)

// nonInteractiveDefaults keep tools from waiting on a terminal or an
// editor: each command runs with stdin at /dev/null and, under Setsid, no
// controlling terminal, so anything that would prompt must fail instead.
var nonInteractiveDefaults = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_EDITOR=true",
	"PAGER=cat",
	"GIT_PAGER=cat",
	"DEBIAN_FRONTEND=noninteractive",
}

// NonInteractiveEnv returns env plus every default whose KEY env does not
// already set. A nil env means "inherit the process environment" and is
// expanded from os.Environ() first. An agent- or operator-supplied value
// always wins.
//
// KEY is the text before the first '='. The returned slice is always a new
// slice: it never appends into the caller's backing array, so a caller that
// reuses env afterwards sees it unchanged.
func NonInteractiveEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	set := make(map[string]bool, len(env))
	for _, kv := range env {
		key := kv
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			key = kv[:eq]
		}
		set[key] = true
	}
	out := make([]string, 0, len(env)+len(nonInteractiveDefaults))
	out = append(out, env...)
	for _, def := range nonInteractiveDefaults {
		key := def
		if eq := strings.IndexByte(def, '='); eq >= 0 {
			key = def[:eq]
		}
		if !set[key] {
			out = append(out, def)
		}
	}
	return out
}
