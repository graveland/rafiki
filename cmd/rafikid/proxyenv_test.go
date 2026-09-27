package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
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
func TestProxyChildEnv_NothingWhenNoFaceAndNoOverride(t *testing.T) {
	t.Setenv(paths.URL, "")
	ctl := &Controller{} // no face started
	for _, kind := range []string{protocol.KindClaude, protocol.KindFundi} {
		env, vals := ctl.proxyChildEnv(protocol.SpawnRequest{Kind: kind}, "c_1")
		assert.NewCollecting(t).False(env != nil || vals.MCPConfig != "" || vals.ModelArgs != nil, "kind %q with no proxy configured: got env %v / vals %+v, want neither", kind, env, vals)
	}
}

// The fundi kind reaches rafiki in-process; pointing it at an HTTP face would
// put a network hop in front of a library call.
func TestProxyChildEnv_NeverRoutesAgent(t *testing.T) {
	ctl := &Controller{proxyURL: "http://127.0.0.1:1", proxyToken: "t"}
	env, vals := ctl.proxyChildEnv(protocol.SpawnRequest{Kind: protocol.KindFundi}, "c_1")
	assert.NewCollecting(t).False(env != nil || vals.MCPConfig != "" || vals.ModelArgs != nil, "fundi kind was routed through the proxy: env %v / vals %+v", env, vals)
}

func TestProxyChildEnv_Claude(t *testing.T) {
	// An ambient RAFIKI_URL (e.g. a dev shell pointed at a remote daemon)
	// deliberately overrides ctl.proxyURL — isolate from it so this test's
	// outcome doesn't depend on who is running it.
	t.Setenv(paths.URL, "")
	c := assert.NewCollecting(t)
	ctl := &Controller{proxyURL: "http://localhost:8035", proxyToken: "tok"}

	env, vals := ctl.proxyChildEnv(protocol.SpawnRequest{Kind: protocol.KindClaude, Model: "glm-5.2"}, "c_abc")
	envMap := envKeys(env)
	c.Eq("http://localhost:8035", envMap["ANTHROPIC_BASE_URL"], "ANTHROPIC_BASE_URL =")
	c.Eq("tok", envMap["ANTHROPIC_AUTH_TOKEN"], "ANTHROPIC_AUTH_TOKEN =")
	// The model must travel as a custom option: ANTHROPIC_MODEL is validated
	// client-side against an Anthropic allowlist and would reject a slash id
	// before the request ever left.
	c.Eq("glm-5.2", envMap["ANTHROPIC_CUSTOM_MODEL_OPTION"], "ANTHROPIC_CUSTOM_MODEL_OPTION =")
	_, ok := envMap["ANTHROPIC_MODEL"]
	c.False(ok, "ANTHROPIC_MODEL set")
	// Headers are newline-separated — the only separator Claude Code accepts.
	h := envMap["ANTHROPIC_CUSTOM_HEADERS"]
	c.False(!strings.Contains(h, "X-Rafiki-Session: c_abc") || !strings.Contains(h, "X-Rafiki-Source: claude"), "ANTHROPIC_CUSTOM_HEADERS = %q", h)
	c.StrContains(h, "\n", "headers not newline-separated; a comma silently collapses them into one")
	// The per-child MCP secret travels as RAFIKI_MCP_TOKEN and is distinct
	// from the proxy bearer (which stays the per-boot secret): the two are
	// different credentials with different lifetimes, and the MCP config's
	// Authorization placeholder expands from this variable.
	c.NotEq("", envMap["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN not set; the child's MCP calls would authenticate as nobody")
	c.NotEq(envMap["ANTHROPIC_AUTH_TOKEN"], envMap["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN == the proxy bearer; the per-child credential must not collapse into it")
	// The MCP config carries the placeholder, never the token itself: argv is
	// world-readable via ps on this machine.
	c.NotStrContains(vals.MCPConfig, envMap["RAFIKI_MCP_TOKEN"], "vals.MCPConfig carries the raw MCP secret; it must hold only the ${RAFIKI_MCP_TOKEN} placeholder")
	// A rebuild for the SAME child must reuse the secret (resume/respawn
	// rebuild the spawn env through this path; a fresh mint would orphan the
	// credential the running child holds).
	env2, _ := ctl.proxyChildEnv(protocol.SpawnRequest{Kind: protocol.KindClaude, Model: "glm-5.2"}, "c_abc")
	c.Eq(envMap["RAFIKI_MCP_TOKEN"], envKeys(env2)["RAFIKI_MCP_TOKEN"], "second proxyChildEnv for the same child minted a different MCP secret")

	// vals carries the argv decisions as data now: MCPConfig is the BARE
	// inline JSON (buildClaudeArgv feeds it to claudeargv.Params.MCPConfig,
	// whose Build prepends the flag — a rendered element here would come out
	// doubled) and, with Model set, the --model pair that buildClaudeArgv puts
	// in the child's argv as ModelArgs — REPLACING the plain pair, so the child
	// carries exactly one --model.
	if !strings.HasPrefix(vals.MCPConfig, "{") {
		t.Errorf("vals.MCPConfig = %q, want the bare inline JSON document", vals.MCPConfig)
	}
	c.EqDiff([]string{"--model", "glm-5.2"}, vals.ModelArgs, "vals.ModelArgs")
}

func TestProxyRoutesKind(t *testing.T) {
	t.Setenv(paths.ProxyKinds, "")
	c := assert.NewCollecting(t)
	for _, k := range []string{protocol.KindClaude} {
		c.True(proxyRoutesKind(k), "default must route %q", k)
	}
	// The escape hatch: narrowing the list makes "is it the proxy?" answerable
	// with a restart instead of a rebuild.
	t.Setenv(paths.ProxyKinds, protocol.KindClaude)
	c.False(proxyRoutesKind(protocol.KindFundi), "fundi routed despite being excluded by RAFIKI_PROXY_KINDS")
	c.True(proxyRoutesKind(protocol.KindClaude), "claude not routed despite being listed")
}

// The embedded face is the default path; RAFIKI_URL redirects children at
// an external rafiki instead, taking its token from the environment file rather
// than the per-boot one the face generated for itself.
func TestProxyChildEnv_ExplicitURLOverridesTheEmbeddedFace(t *testing.T) {
	c := assert.NewCollecting(t)
	ctl := &Controller{proxyURL: "http://127.0.0.1:58318", proxyToken: "boot-token"}
	t.Setenv(paths.URL, "http://shared-capture:8035")
	t.Setenv(paths.Token, "shared-token")

	env, _ := ctl.proxyChildEnv(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c_1")
	envMap := envKeys(env)
	c.Eq("http://shared-capture:8035", envMap["ANTHROPIC_BASE_URL"], "ANTHROPIC_BASE_URL")
	c.Eq("shared-token", envMap["ANTHROPIC_AUTH_TOKEN"], "token")
}
