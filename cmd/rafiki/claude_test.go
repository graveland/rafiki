// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
)

// envMap turns the assembled environment into a lookup, and reports which keys
// were set more than once — a duplicate would mean an inherited copy survived
// alongside the one this launcher sets, and the last-wins behaviour of execve
// makes that silent.
func envMap(t *testing.T, env []string) (map[string]string, []string) {
	t.Helper()
	out := make(map[string]string, len(env))
	seen := make(map[string]int, len(env))
	var dupes []string
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		seen[k]++
		if seen[k] == 2 {
			dupes = append(dupes, k)
		}
		out[k] = v
	}
	return out, dupes
}

func TestBuildClaudeInvocation_SetsProxyEnv(t *testing.T) {
	inv := buildClaudeInvocation(nil, "http://localhost:8035", "tok", "sess-1", "", 0, nil)
	env, dupes := envMap(t, inv.Env)
	if len(dupes) != 0 {
		t.Errorf("duplicate env keys: %v", dupes)
	}
	if got := env["ANTHROPIC_BASE_URL"]; got != "http://localhost:8035" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := env["ANTHROPIC_AUTH_TOKEN"]; got != "tok" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q", got)
	}
	if got := env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Rafiki-Session: sess-1" {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q", got)
	}
}

// Nesting protection: launching a session from inside one must not inherit the
// outer session's base URL, model or correlation header, or the child's turns
// land on the parent's captured conversation.
func TestBuildClaudeInvocation_StripsInheritedManagedVars(t *testing.T) {
	parent := []string{
		"ANTHROPIC_BASE_URL=http://stale:1111",
		"ANTHROPIC_AUTH_TOKEN=stale-token",
		"ANTHROPIC_CUSTOM_HEADERS=X-Rafiki-Session: OUTER",
		"ANTHROPIC_CUSTOM_MODEL_OPTION=stale-model",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=rafiki: stale-model",
		"ANTHROPIC_MODEL=stale-model",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW=999",
		"PATH=/usr/bin",
	}
	inv := buildClaudeInvocation(parent, "http://localhost:8035", "tok", "inner", "", 0, nil)
	env, dupes := envMap(t, inv.Env)
	if len(dupes) != 0 {
		t.Errorf("inherited copies survived alongside the new values: %v", dupes)
	}
	if got := env["ANTHROPIC_BASE_URL"]; got != "http://localhost:8035" {
		t.Errorf("stale base URL survived: %q", got)
	}
	if got := env["ANTHROPIC_CUSTOM_HEADERS"]; !strings.Contains(got, "inner") {
		t.Errorf("stale session header survived: %q", got)
	}
	// No model was requested, so every model var must be absent entirely —
	// not merely overwritten.
	for _, k := range []string{"ANTHROPIC_CUSTOM_MODEL_OPTION", "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "ANTHROPIC_MODEL", "CLAUDE_CODE_AUTO_COMPACT_WINDOW"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s should be absent when no model is requested, got %q", k, env[k])
		}
	}
	if env["PATH"] != "/usr/bin" { // unrelated vars pass through untouched
		t.Errorf("PATH = %q, want /usr/bin", env["PATH"])
	}
}

// The real Anthropic key must never reach a proxied child: Claude Code sends it
// as x-api-key, which bypasses the bearer the proxy authenticates on and
// defeats the capture the proxy exists for.
func TestBuildClaudeInvocation_StripsProviderKeys(t *testing.T) {
	parent := []string{"ANTHROPIC_API_KEY=sk-ant-real", "OPENROUTER_API_KEY=sk-or-real", "HOME=/home/u"}
	inv := buildClaudeInvocation(parent, "http://localhost:8035", "tok", "s", "", 0, nil)
	env, _ := envMap(t, inv.Env)
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s leaked to the child: %q", k, v)
		}
	}
	if env["HOME"] != "/home/u" {
		t.Errorf("HOME = %q", env["HOME"])
	}
}

// The whole point of the custom-model-option dance: ANTHROPIC_MODEL and a bare
// --model are validated against Claude Code's client-side allowlist and reject
// non-Anthropic ids before any request leaves, so a slash id must travel as a
// registered custom option instead.
func TestBuildClaudeInvocation_ModelUsesCustomOption(t *testing.T) {
	inv := buildClaudeInvocation(nil, "http://x", "tok", "s", "moonshotai/kimi-k3", 0, nil)
	env, _ := envMap(t, inv.Env)
	if got := env["ANTHROPIC_CUSTOM_MODEL_OPTION"]; got != "moonshotai/kimi-k3" {
		t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION = %q", got)
	}
	if got := env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"]; got != "rafiki: moonshotai/kimi-k3" {
		t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME = %q", got)
	}
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL must not be set — it is allowlist-validated and would reject the id")
	}
	if !slices.Equal(inv.Args, []string{"--model", "moonshotai/kimi-k3"}) {
		t.Errorf("Args = %v, want the --model pair activating the registered option", inv.Args)
	}
}

func TestBuildClaudeInvocation_AutoCompactWindow(t *testing.T) {
	t.Run("set with a model", func(t *testing.T) {
		inv := buildClaudeInvocation(nil, "http://x", "t", "s", "glm-5.2", 180000, nil)
		env, _ := envMap(t, inv.Env)
		if got := env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; got != "180000" {
			t.Errorf("CLAUDE_CODE_AUTO_COMPACT_WINDOW = %q, want 180000", got)
		}
	})
	t.Run("omitted when unknown", func(t *testing.T) {
		// 0 means the catalog could not resolve a window; Claude Code's own
		// default must be left alone rather than pinned to something invented.
		inv := buildClaudeInvocation(nil, "http://x", "t", "s", "glm-5.2", 0, nil)
		env, _ := envMap(t, inv.Env)
		if _, ok := env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; ok {
			t.Error("window pinned despite an unresolved context length")
		}
	})
	t.Run("omitted without a model", func(t *testing.T) {
		inv := buildClaudeInvocation(nil, "http://x", "t", "s", "", 180000, nil)
		env, _ := envMap(t, inv.Env)
		if _, ok := env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; ok {
			t.Error("window pinned with no model to pin it for")
		}
	})
}

func TestBuildClaudeInvocation_PassthroughArgs(t *testing.T) {
	t.Run("with a model", func(t *testing.T) {
		inv := buildClaudeInvocation(nil, "http://x", "t", "s", "opus-latest", 0,
			[]string{"--permission-mode", "plan"})
		want := []string{"--model", "opus-latest", "--permission-mode", "plan"}
		if !slices.Equal(inv.Args, want) {
			t.Errorf("Args = %v, want %v", inv.Args, want)
		}
	})
	t.Run("without a model", func(t *testing.T) {
		inv := buildClaudeInvocation(nil, "http://x", "t", "s", "", 0, []string{"--continue"})
		if !slices.Equal(inv.Args, []string{"--continue"}) {
			t.Errorf("Args = %v, want just the passthrough", inv.Args)
		}
	})
}

func TestBuildClaudeInvocation_NoSessionHeaderWhenEmpty(t *testing.T) {
	inv := buildClaudeInvocation(nil, "http://x", "t", "", "", 0, nil)
	env, _ := envMap(t, inv.Env)
	if v, ok := env["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		// An empty id would produce a malformed header rather than no header.
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS set to %q for an empty session id", v)
	}
}
