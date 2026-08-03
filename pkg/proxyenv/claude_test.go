// SPDX-License-Identifier: Apache-2.0

package proxyenv

import (
	"slices"
	"strings"
	"testing"
)

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

// An empty URL means "not proxied": the caller's environment must come back
// untouched, so enabling this feature cannot break an install that has not
// configured a proxy.
func TestClaude_NoURLIsPassthrough(t *testing.T) {
	in := []string{"ANTHROPIC_API_KEY=sk-real", "ANTHROPIC_BASE_URL=http://inherited", "HOME=/h"}
	env, args := Claude(in, ClaudeOptions{})
	if !slices.Equal(env, in) {
		t.Errorf("env = %v, want it unchanged", env)
	}
	if args != nil {
		t.Errorf("args = %v, want none", args)
	}
}

func TestClaude_SetsProxyAndStrips(t *testing.T) {
	in := []string{
		"ANTHROPIC_API_KEY=sk-real",
		"OPENROUTER_API_KEY=sk-or",
		"ANTHROPIC_BASE_URL=http://stale",
		"ANTHROPIC_CUSTOM_HEADERS=X-Rafiki-Session: OUTER",
		"ANTHROPIC_MODEL=stale",
		"HOME=/h",
	}
	env, _ := Claude(in, ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	got, dupes := envMap(t, env)
	if len(dupes) != 0 {
		t.Errorf("inherited copies survived alongside the new values: %v", dupes)
	}
	for _, k := range Credentials {
		if _, ok := got[k]; ok {
			t.Errorf("%s leaked to a proxied child", k)
		}
	}
	if got["ANTHROPIC_BASE_URL"] != "http://localhost:8035" {
		t.Errorf("stale base URL survived: %q", got["ANTHROPIC_BASE_URL"])
	}
	if _, ok := got["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL must never be set — it is allowlist-validated client-side")
	}
	if got["HOME"] != "/h" {
		t.Errorf("unrelated variable lost: HOME=%q", got["HOME"])
	}
}

// Claude Code refuses to send to a custom base URL without an auth token, so an
// empty one must become a placeholder rather than being omitted.
func TestClaude_EmptyTokenGetsPlaceholder(t *testing.T) {
	env, _ := Claude(nil, ClaudeOptions{URL: "http://x"})
	got, _ := envMap(t, env)
	if got["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Error("ANTHROPIC_AUTH_TOKEN empty; Claude Code will not send to a custom base URL")
	}
}

// A proxied child gets _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL so Claude
// Code's byte watchdog (which lets SSE pings feed the stream idle watchdog)
// stays on despite the non-Anthropic base URL host.
func TestClaude_DefaultsFirstPartyAssume(t *testing.T) {
	env, _ := Claude(nil, ClaudeOptions{URL: "http://x"})
	got, _ := envMap(t, env)
	if got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"] != "1" {
		t.Errorf("_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL = %q, want 1", got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"])
	}
}

// An explicit inherited value is a deliberate choice and must survive — a
// default that overrode it would make distrusting a proxy impossible.
func TestClaude_InheritedDefaultWins(t *testing.T) {
	in := []string{"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=0", "HOME=/h"}
	env, _ := Claude(in, ClaudeOptions{URL: "http://x"})
	got, dupes := envMap(t, env)
	if got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"] != "0" {
		t.Errorf("explicit value overridden: %q", got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"])
	}
	if slices.Contains(dupes, "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL") {
		t.Error("default appended alongside the inherited value")
	}
}

func TestClaude_ModelUsesCustomOption(t *testing.T) {
	env, args := Claude(nil, ClaudeOptions{URL: "http://x", Model: "moonshotai/kimi-k3"})
	got, _ := envMap(t, env)
	if got["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "moonshotai/kimi-k3" {
		t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION = %q", got["ANTHROPIC_CUSTOM_MODEL_OPTION"])
	}
	if _, ok := got["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL set; it would be rejected before the request leaves")
	}
	if !slices.Equal(args, []string{"--model", "moonshotai/kimi-k3"}) {
		t.Errorf("args = %v, want the --model pair activating the registered option", args)
	}
}

func TestClaude_AutoCompactOnlyWithModel(t *testing.T) {
	env, _ := Claude(nil, ClaudeOptions{URL: "http://x", AutoCompactWindow: 180000})
	got, _ := envMap(t, env)
	if _, ok := got["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; ok {
		t.Error("window pinned with no model to pin it for")
	}
	env, _ = Claude(nil, ClaudeOptions{URL: "http://x", Model: "m", AutoCompactWindow: 180000})
	got, _ = envMap(t, env)
	if got["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "180000" {
		t.Errorf("CLAUDE_CODE_AUTO_COMPACT_WINDOW = %q, want 180000", got["CLAUDE_CODE_AUTO_COMPACT_WINDOW"])
	}
}

// The separator is a literal newline and nothing else: a comma or an escaped
// \n silently collapses into one malformed header, and correlation just stops
// working with no error anywhere.
func TestFormatHeaders_NewlineSeparatedAndSorted(t *testing.T) {
	got := FormatHeaders(map[string]string{
		"X-Rafiki-Source":  "claude",
		"X-Rafiki-Session": "sess-1",
	})
	want := "X-Rafiki-Session: sess-1\nX-Rafiki-Source: claude"
	if got != want {
		t.Errorf("got %q, want %q (newline-separated, sorted)", got, want)
	}
	if strings.Contains(got, ",") || strings.Contains(got, `\n`) {
		t.Error("used a separator Claude Code does not accept")
	}
}

func TestFormatHeaders_Empty(t *testing.T) {
	if got := FormatHeaders(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// A value containing a newline would forge an extra header the proxy would read
// as real; such a header is malformed anyway, so it is dropped.
func TestFormatHeaders_DropsForgedHeaders(t *testing.T) {
	got := FormatHeaders(map[string]string{
		"X-Good": "fine",
		"X-Bad":  "a\nX-Injected: evil",
	})
	if strings.Contains(got, "X-Injected") {
		t.Errorf("header injection through a value: %q", got)
	}
	if got != "X-Good: fine" {
		t.Errorf("got %q, want only the good header", got)
	}
}
