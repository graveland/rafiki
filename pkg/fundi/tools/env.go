// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"os"
	"strings"
)

// EnvGet returns the value of key in an environ slice ("KEY=VALUE" entries,
// the exec.Cmd.Env shape), or "" when absent. An empty key's value is
// indistinguishable from absence — the same contract os.Getenv has, and the
// reason callers that care use envHas-style checks on the key, not on the
// value. env is ToolOpts.Env (nil = the caller's own process environment, but
// nil is handled by the CALLER: os.Getenv there).
func EnvGet(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// getenv is a tool's view of one variable in ITS environment: the pinned
// env (ToolOpts.Env) when one was built with the tool, else the serving
// process's environment — the zero-value contract every bare ToolOpts{}
// caller and existing test relies on. Never the merged view of another
// child's forwarded env; each tool sees only its own pinned slice.
func getenv(env []string, key string) string {
	if env == nil {
		return os.Getenv(key)
	}
	return EnvGet(env, key)
}

// MergeEnv overlays overlay on base (both environ slices): every KEY in
// overlay wins with overlay's value, keys only in base pass through, order
// and duplicates from both are preserved aside from the shadowed entries.
// This is the per-child environment join: base is the serving process's
// environment (the executor's, pinned at startup — see ToolOpts.Env), overlay
// is what one caller forwarded. Callers that need the opposite precedence
// swap the arguments; nothing here special-cases any key name.
func MergeEnv(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	shadowed := make(map[string]bool, len(overlay))
	for _, kv := range overlay {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		shadowed[kv[:eq]] = true
	}
	out := make([]string, 0, len(base)+len(overlay))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 && shadowed[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, overlay...)
}

// AgentEnv is the environment a child's tool subprocesses run under: base
// (nil = the process environment, the same contract as ToolOpts.Env) with the
// markers that say an agent is driving. AI_AGENT is the cross-tool convention
// for "a program is running this, not a person at a terminal"; RAFIKI_CHILD_ID
// is the child's own id, what `rafikid agent --ref` defaults from. Both are
// applied LAST, so neither an inherited nor a caller-forwarded value can
// shadow them. An empty childID omits RAFIKI_CHILD_ID rather than setting it
// empty.
func AgentEnv(base []string, childID string) []string {
	if base == nil {
		base = os.Environ()
	}
	markers := []string{"AI_AGENT=rafiki"}
	if childID != "" {
		markers = append(markers, "RAFIKI_CHILD_ID="+childID)
	}
	return MergeEnv(base, markers)
}
