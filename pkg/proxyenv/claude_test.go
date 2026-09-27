// SPDX-License-Identifier: Apache-2.0

package proxyenv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
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
func TestClaudeEnv_NoURLIsPassthrough(t *testing.T) {
	c := assert.NewCollecting(t)
	in := []string{"ANTHROPIC_API_KEY=sk-real", "ANTHROPIC_BASE_URL=http://inherited", "HOME=/h"}
	env, v := ClaudeEnv(in, ClaudeOptions{})
	c.EqDiff(in, env, "env")
	c.False(v.MCPConfig != "" || len(v.ModelArgs) > 0, "values = %+v, want none", v)
}

func TestClaudeEnv_SetsProxyAndStrips(t *testing.T) {
	c := assert.NewCollecting(t)
	in := []string{
		"ANTHROPIC_API_KEY=sk-real",
		"OPENROUTER_API_KEY=sk-or",
		"ANTHROPIC_BASE_URL=http://stale",
		"ANTHROPIC_CUSTOM_HEADERS=X-Rafiki-Session: OUTER",
		"ANTHROPIC_MODEL=stale",
		"RAFIKI_MCP_TOKEN=OUTER",
		"HOME=/h",
	}
	env, _ := ClaudeEnv(in, ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	got, dupes := envMap(t, env)
	c.Empty(dupes, "inherited copies survived alongside the new values")
	for _, k := range Credentials {
		_, ok := got[k]
		c.False(ok, "%s leaked to a proxied child", k)
	}
	c.Eq("http://localhost:8035", got["ANTHROPIC_BASE_URL"], "stale base URL survived")
	_, ok := got["ANTHROPIC_MODEL"]
	c.False(ok, "ANTHROPIC_MODEL must never be set — it is allowlist-validated client-side")
	c.Eq("tok", got["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN")
	c.Eq("/h", got["HOME"], "unrelated variable lost: HOME=")
}

// Claude Code refuses to send to a custom base URL without an auth token, so an
// empty one must become a placeholder rather than being omitted.
func TestClaudeEnv_EmptyTokenGetsPlaceholder(t *testing.T) {
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x"})
	got, _ := envMap(t, env)
	assert.NewCollecting(t).NotEq("", got["ANTHROPIC_AUTH_TOKEN"], "ANTHROPIC_AUTH_TOKEN empty; Claude Code will not send to a custom base URL")
}

// A proxied child gets _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL so Claude
// Code's byte watchdog (which lets SSE pings feed the stream idle watchdog)
// stays on despite the non-Anthropic base URL host.
func TestClaudeEnv_DefaultsFirstPartyAssume(t *testing.T) {
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x"})
	got, _ := envMap(t, env)
	assert.NewCollecting(t).Eq("1", got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"], "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL")
}

// An explicit inherited value is a deliberate choice and must survive — a
// default that overrode it would make distrusting a proxy impossible.
func TestClaudeEnv_InheritedDefaultWins(t *testing.T) {
	c := assert.NewCollecting(t)
	in := []string{"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=0", "HOME=/h"}
	env, _ := ClaudeEnv(in, ClaudeOptions{URL: "http://x"})
	got, dupes := envMap(t, env)
	c.Eq("0", got["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"], "explicit value overridden")
	c.NotContains(dupes, "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "default appended alongside the inherited value")
}

func TestClaudeEnv_ModelUsesCustomOption(t *testing.T) {
	c := assert.NewCollecting(t)
	env, v := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Model: "moonshotai/kimi-k3"})
	got, _ := envMap(t, env)
	c.Eq("moonshotai/kimi-k3", got["ANTHROPIC_CUSTOM_MODEL_OPTION"], "ANTHROPIC_CUSTOM_MODEL_OPTION =")
	_, ok := got["ANTHROPIC_MODEL"]
	c.False(ok, "ANTHROPIC_MODEL set; it would be rejected before the request leaves")
	// Values.ModelArgs is the --model pair the argv producer appends — it
	// REPLACES the plain pair, so the child carries exactly one --model.
	wantPair := []string{"--model", "moonshotai/kimi-k3"}
	c.EqDiff(wantPair, v.ModelArgs, "Values.ModelArgs")
}

// Gated on URL, not Model: a session with no model chosen must still get
// agent control, or a bare "rafiki claude" (no --model) would silently lose
// the surface.
func TestClaudeEnv_MCPConfigPresentWithNoModel(t *testing.T) {
	_, v := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "tok"})
	assert.NewCollecting(t).NotEq("", v.MCPConfig, "Values = %+v, want MCPConfig set even with no model", v)
}

func TestClaudeEnv_MCPConfigJSONShape(t *testing.T) {
	c := assert.NewCollecting(t)
	env, v := ClaudeEnv(nil, ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	got, _ := envMap(t, env)
	c.Eq("tok", got["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN")
	c.Require().NotEq("", v.MCPConfig, "Values.MCPConfig empty for a proxied session")
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	err := json.Unmarshal([]byte(v.MCPConfig), &doc)
	c.Require().NoError(err, "Values.MCPConfig is not valid JSON: %v (%s)", err, v.MCPConfig)
	rafiki, ok := doc.MCPServers["rafiki"]
	c.Require().True(ok, "mcpServers = %v, missing \"rafiki\" key", doc.MCPServers)
	c.Eq("http", rafiki.Type, "type")
	c.Eq("http://localhost:8035/mcp", rafiki.URL, "url")
	c.Eq("Bearer ${RAFIKI_MCP_TOKEN}", rafiki.Headers["Authorization"], "Authorization header")
}

// No URL means unproxied: RAFIKI_MCP_TOKEN must not appear from nowhere.
func TestClaudeEnv_MCPTokenAbsentWhenUnproxied(t *testing.T) {
	c := assert.NewCollecting(t)
	env, v := ClaudeEnv([]string{"HOME=/h"}, ClaudeOptions{})
	got, _ := envMap(t, env)
	_, ok := got["RAFIKI_MCP_TOKEN"]
	c.False(ok, "RAFIKI_MCP_TOKEN set with no URL configured")
	c.False(v.MCPConfig != "" || len(v.ModelArgs) > 0, "values = %+v, want none", v)
}

// _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL suppresses Claude Code's own
// "custom base URL, so no deferred tools" guard, so a non-Anthropic model
// would be sent tool_reference blocks it cannot resolve and the first turn
// would 400. Every non-Anthropic spelling must therefore turn tool search off
// explicitly.
func TestClaudeEnv_ToolSearchDisabledForNonAnthropicModels(t *testing.T) {
	for _, model := range []string{"moonshotai/kimi-k3", "kimi-k3", "glm-5.2", "z-ai/glm-5.2", "~openai/gpt-latest"} {
		env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Model: model})
		got, _ := envMap(t, env)
		assert.NewCollecting(t).Eq("false", got["ENABLE_TOOL_SEARCH"], "model %q: ENABLE_TOOL_SEARCH = %q, want false", model, got["ENABLE_TOOL_SEARCH"])
	}
}

// Anthropic models keep the feature: rafiki forwards tool_reference blocks
// untouched, so a proxied session should behave like a direct one.
func TestClaudeEnv_ToolSearchLeftAloneForAnthropicModels(t *testing.T) {
	for _, model := range []string{"", "claude-opus-5", "opus-latest", "anthropic/claude-opus-5", "~anthropic/claude-opus-latest"} {
		env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Model: model})
		got, _ := envMap(t, env)
		v, ok := got["ENABLE_TOOL_SEARCH"]
		assert.NewCollecting(t).False(ok, "model %q: ENABLE_TOOL_SEARCH = %q, want unset", model, v)
	}
}

// An explicit setting is the user's call — including "yes, my proxy forwards
// tool_reference to this model, leave it on".
func TestClaudeEnv_ToolSearchInheritedValueWins(t *testing.T) {
	c := assert.NewCollecting(t)
	in := []string{"ENABLE_TOOL_SEARCH=auto:50", "HOME=/h"}
	env, _ := ClaudeEnv(in, ClaudeOptions{URL: "http://x", Model: "moonshotai/kimi-k3"})
	got, dupes := envMap(t, env)
	c.Eq("auto:50", got["ENABLE_TOOL_SEARCH"], "explicit value overridden")
	c.NotContains(dupes, "ENABLE_TOOL_SEARCH", "appended alongside the inherited value")
}

func TestClaudeEnv_AutoCompactOnlyWithModel(t *testing.T) {
	c := assert.NewCollecting(t)
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", AutoCompactWindow: 180000})
	got, _ := envMap(t, env)
	_, ok := got["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]
	c.False(ok, "window pinned with no model to pin it for")
	env, _ = ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Model: "m", AutoCompactWindow: 180000})
	got, _ = envMap(t, env)
	c.Eq("180000", got["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "CLAUDE_CODE_AUTO_COMPACT_WINDOW")
}

// The separator is a literal newline and nothing else: a comma or an escaped
// \n silently collapses into one malformed header, and correlation just stops
// working with no error anywhere.
func TestFormatHeaders_NewlineSeparatedAndSorted(t *testing.T) {
	c := assert.NewCollecting(t)
	got := FormatHeaders(map[string]string{
		"X-Rafiki-Source":  "claude",
		"X-Rafiki-Session": "sess-1",
	})
	want := "X-Rafiki-Session: sess-1\nX-Rafiki-Source: claude"
	c.Eq(want, got, "got")
	c.False(strings.Contains(got, ",") || strings.Contains(got, `\n`), "used a separator Claude Code does not accept")
}

func TestFormatHeaders_Empty(t *testing.T) {
	assert.NewCollecting(t).Eq("", FormatHeaders(nil), "got")
}

// A value containing a newline would forge an extra header the proxy would read
// as real; such a header is malformed anyway, so it is dropped.
func TestFormatHeaders_DropsForgedHeaders(t *testing.T) {
	c := assert.NewCollecting(t)
	got := FormatHeaders(map[string]string{
		"X-Good": "fine",
		"X-Bad":  "a\nX-Injected: evil",
	})
	c.NotStrContains(got, "X-Injected", "header injection through a value")
	c.Eq("X-Good: fine", got, "got")
}

// Passthrough is defined by the ABSENCE of ANTHROPIC_AUTH_TOKEN: that variable
// is what makes Claude Code use API-key auth instead of falling through to its
// OAuth subscription. Verified against claude-cli 2.1.226.
func TestClaudeEnv_PassthroughOmitsAuthToken(t *testing.T) {
	c := assert.NewCollecting(t)
	in := []string{"ANTHROPIC_API_KEY=sk-real", "HOME=/h"}
	env, _ := ClaudeEnv(in, ClaudeOptions{
		URL:             "http://localhost:8035",
		Token:           "dev",
		PassthroughAuth: true,
		Headers:         map[string]string{"X-Rafiki-Session": "s1"},
	})
	got, dupes := envMap(t, env)
	c.LessOrEqual(0, len(dupes), "duplicate variables: %v", dupes)
	if v, ok := got["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want it absent entirely", v)
	}
	_, ok := got["ANTHROPIC_API_KEY"]
	c.False(ok, "ANTHROPIC_API_KEY survived; it must be stripped or it outranks OAuth")
	c.Eq("http://localhost:8035", got["ANTHROPIC_BASE_URL"], "ANTHROPIC_BASE_URL =")
	c.StrContains(got["ANTHROPIC_CUSTOM_HEADERS"], "X-Rafiki-Token: dev", "ANTHROPIC_CUSTOM_HEADERS")
	c.StrContains(got["ANTHROPIC_CUSTOM_HEADERS"], "X-Rafiki-Session: s1", "ANTHROPIC_CUSTOM_HEADERS")
}

// The caller's Headers map must not be mutated: callers reuse it, and a
// surprise credential appearing in it is the kind of aliasing bug that only
// shows up in the second session.
func TestClaudeEnv_PassthroughDoesNotMutateCallerHeaders(t *testing.T) {
	headers := map[string]string{"X-Rafiki-Session": "s1"}
	_, _ = ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "dev", PassthroughAuth: true, Headers: headers})
	_, ok := headers["X-Rafiki-Token"]
	assert.NewCollecting(t).False(ok, "ClaudeEnv mutated the caller's Headers map")
}

// Without the option, nothing changes: the token stays in ANTHROPIC_AUTH_TOKEN
// and no X-Rafiki-Token header is emitted.
func TestClaudeEnv_NoPassthroughKeepsAuthToken(t *testing.T) {
	c := assert.NewCollecting(t)
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "dev"})
	got, _ := envMap(t, env)
	c.Eq("dev", got["ANTHROPIC_AUTH_TOKEN"], "ANTHROPIC_AUTH_TOKEN")
	c.NotStrContains(got["ANTHROPIC_CUSTOM_HEADERS"], "X-Rafiki-Token", "ANTHROPIC_CUSTOM_HEADERS")
}

// The child paths must be able to split the two credentials: the proxy bearer
// (which billing attribution keys on) stays Token, while the MCP config
// authenticates as the child. In the default path the bearer is
// ANTHROPIC_AUTH_TOKEN; under PassthroughAuth it moves to the X-Rafiki-Token
// header — both must keep the proxy value while RAFIKI_MCP_TOKEN carries the
// child's.
func TestMCPTokenOverridesToken(t *testing.T) {
	c := assert.NewCollecting(t)
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "proxy", MCPToken: "child"})
	got, _ := envMap(t, env)
	c.Eq("child", got["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN")
	c.Eq("proxy", got["ANTHROPIC_AUTH_TOKEN"], "ANTHROPIC_AUTH_TOKEN")

	env, _ = ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "proxy", MCPToken: "child", PassthroughAuth: true})
	got, _ = envMap(t, env)
	c.Eq("child", got["RAFIKI_MCP_TOKEN"], "passthrough: RAFIKI_MCP_TOKEN")
	c.StrContains(got["ANTHROPIC_CUSTOM_HEADERS"], "X-Rafiki-Token: proxy", "ANTHROPIC_CUSTOM_HEADERS")
}

// Empty MCPToken falls back to Token, so the interactive path — where the two
// credentials are the same user token — behaves exactly as it did before the
// field existed.
func TestMCPTokenFallsBackToToken(t *testing.T) {
	env, _ := ClaudeEnv(nil, ClaudeOptions{URL: "http://x", Token: "tok"})
	got, _ := envMap(t, env)
	assert.NewCollecting(t).Eq("tok", got["RAFIKI_MCP_TOKEN"], "RAFIKI_MCP_TOKEN")
}

// Values.MCPConfig is the BARE inline JSON — the shape claudeargv.Params.MCPConfig
// expects, with Build itself prepending the flag. The pre-fix producer assigned
// the rendered element, so every consumer that fed the value to Params verbatim
// (the daraja standalone host, `rafiki claude`) emitted
// --mcp-config=--mcp-config={...} and only a compensating TrimPrefix shim kept
// the local-subprocess path correct.
//
// That there is exactly ONE --mcp-config element in the final argv is the argv
// producers' property now (claudeargv.Build renders the flag once over the
// bare value) — pinned in pkg/claudeargv and, across both spawn paths, by
// test/integration's TestClaudeArgvIdenticalAcrossPaths. Here the contract is
// the value's shape alone.
func TestClaudeEnvMCPConfigIsBareJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	_, v := ClaudeEnv(nil, ClaudeOptions{URL: "http://localhost:8035", Token: "tok"})
	c.Require().NotEq("", v.MCPConfig, "Values.MCPConfig empty for a proxied session")
	c.False(strings.HasPrefix(v.MCPConfig, "--mcp-config="), "Values.MCPConfig = %q, want the bare JSON document (the flag prefix is the argv renderer's job)", v.MCPConfig)
	if !strings.HasPrefix(v.MCPConfig, "{") {
		t.Errorf("Values.MCPConfig = %q, want an inline JSON document starting with '{'", v.MCPConfig)
	}
	c.True(json.Valid([]byte(v.MCPConfig)), "Values.MCPConfig is not valid JSON: %q", v.MCPConfig)
}
