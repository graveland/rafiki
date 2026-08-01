package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func envKeys(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}

// No proxy configured must mean no behaviour change at all. This is what makes
// the feature safe to ship: an install that has not opted in is untouched.
func TestProxyChildEnv_DisabledByDefault(t *testing.T) {
	t.Setenv(paths.ProxyURL, "")
	for _, kind := range []string{"pi", "claude", "agent"} {
		if got := proxyChildEnv(protocol.SpawnRequest{Kind: kind}, "c_1"); got != nil {
			t.Errorf("kind %q with no proxy configured: got %v, want nothing", kind, got)
		}
	}
}

// The agent kind reaches rafiki in-process; pointing it at an HTTP face would
// put a network hop in front of a library call.
func TestProxyChildEnv_NeverRoutesAgent(t *testing.T) {
	t.Setenv(paths.ProxyURL, "http://localhost:8035")
	if got := proxyChildEnv(protocol.SpawnRequest{Kind: "agent"}, "c_1"); got != nil {
		t.Errorf("agent kind was routed through the proxy: %v", got)
	}
}

func TestProxyChildEnv_Claude(t *testing.T) {
	t.Setenv(paths.ProxyURL, "http://localhost:8035")
	t.Setenv(paths.ProxyToken, "tok")

	env := envKeys(proxyChildEnv(protocol.SpawnRequest{Kind: "claude", Model: "glm-5.2"}, "c_abc"))
	if env["ANTHROPIC_BASE_URL"] != "http://localhost:8035" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q", env["ANTHROPIC_AUTH_TOKEN"])
	}
	// The model must travel as a custom option: ANTHROPIC_MODEL is validated
	// client-side against an Anthropic allowlist and would reject a slash id
	// before the request ever left.
	if env["ANTHROPIC_CUSTOM_MODEL_OPTION"] != "glm-5.2" {
		t.Errorf("ANTHROPIC_CUSTOM_MODEL_OPTION = %q", env["ANTHROPIC_CUSTOM_MODEL_OPTION"])
	}
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL set")
	}
	// Headers are newline-separated — the only separator Claude Code accepts.
	h := env["ANTHROPIC_CUSTOM_HEADERS"]
	if !strings.Contains(h, "X-Rafiki-Session: c_abc") || !strings.Contains(h, "X-Rafiki-Source: claude") {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q", h)
	}
	if !strings.Contains(h, "\n") {
		t.Error("headers not newline-separated; a comma silently collapses them into one")
	}
}

// pi has no ANTHROPIC_BASE_URL equivalent — it reads these in the fundi-helpers
// extension, where its provider override is registered.
func TestProxyChildEnv_Pi(t *testing.T) {
	t.Setenv(paths.ProxyURL, "http://localhost:8035")
	t.Setenv(paths.ProxyToken, "tok")

	env := envKeys(proxyChildEnv(protocol.SpawnRequest{Kind: "pi"}, "c_xyz"))
	if env[paths.ProxyURL] != "http://localhost:8035" || env[paths.ProxyToken] != "tok" {
		t.Errorf("proxy vars not passed to pi: %v", env)
	}
	if env["FUNDI_SESSION_REF"] != "c_xyz" {
		t.Errorf("FUNDI_SESSION_REF = %q, want the child id", env["FUNDI_SESSION_REF"])
	}
	// pi must not be handed Claude Code's variables.
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("pi was given ANTHROPIC_BASE_URL, which means nothing to it")
	}
}

func TestProxyRoutesKind(t *testing.T) {
	t.Setenv(paths.ProxyKinds, "")
	for _, k := range []string{"pi", "claude"} {
		if !proxyRoutesKind(k) {
			t.Errorf("default must route %q", k)
		}
	}
	// The escape hatch: narrowing the list makes "is it the proxy?" answerable
	// with a restart instead of a rebuild.
	t.Setenv(paths.ProxyKinds, "claude")
	if proxyRoutesKind("pi") {
		t.Error("pi routed despite being excluded by FUNDI_PROXY_KINDS")
	}
	if !proxyRoutesKind("claude") {
		t.Error("claude not routed despite being listed")
	}
}
