// SPDX-License-Identifier: Apache-2.0

// Package proxyenv builds the environment that points a Claude Code process at
// a rafiki proxy.
//
// It exists because two callers need byte-identical behaviour and got it wrong
// independently: `rafiki claude` launches Claude Code interactively, and the
// fundi daemon spawns `--kind claude` children. A difference between them shows
// up as a child whose turns land on the wrong conversation, or one that quietly
// bypasses the proxy altogether — neither of which errors.
package proxyenv

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Managed are the variables this package sets. They are stripped from the
// inherited environment before being set, so that launching a session from
// inside one does not adopt the outer session's base URL, model or correlation
// header — which would land the child's turns on the parent's conversation and
// make a nested run read as a continuation of the outer one.
var Managed = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_CUSTOM_MODEL_OPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
	"ANTHROPIC_MODEL",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
	"RAFIKI_MCP_TOKEN",
}

// Credentials must not reach a proxied child. Claude Code presents
// ANTHROPIC_API_KEY as x-api-key, which bypasses the bearer the proxy
// authenticates on and defeats the capture the proxy exists for. The OpenRouter
// key is the server's business and never the client's.
var Credentials = []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"}

// Defaults are appended only when the inherited environment says nothing about
// them: each asserts a fact about the proxy, and an explicit user setting —
// even a disagreeing one — outranks a default.
var Defaults = []string{
	// Claude Code feeds its 300s stream idle watchdog from raw socket bytes
	// (so SSE keep-alive pings count as activity) only when a byte monitor is
	// attached to the response — and it attaches that monitor only when the
	// base URL host is exactly api.anthropic.com. Through any proxy the
	// monitor is skipped, pings stop counting, and a thinking phase with more
	// than 300s between content events dies with "Response stalled
	// mid-stream" even though bytes flowed the whole time. rafiki's
	// /v1/messages face is a byte-faithful passthrough, so declare it
	// first-party and keep the watchdog fed. (Verified against claude
	// v2.1.220: Yd→d6r→T1e host check gating _chunkTimes via uZc/MRg.)
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1",
}

// toolSearchEnv governs Claude Code's deferred-tool ("tool search") mode: it
// omits most tools from tools[] and sends tool_reference blocks the model
// resolves on demand. Only Anthropic models can call a tool that was omitted,
// so an OpenRouter-routed request carrying them comes back as a hard 400
// ("Deferred custom tools are only supported on Anthropic models") on the
// session's very first turn.
//
// Claude Code disables the mode itself behind a custom base URL — but the
// check is Yd(), the SAME predicate _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL
// above overrides for the byte watchdog. Setting that variable therefore
// re-enables tool search as a side effect, and the two features cannot be
// separated from the outside: the only remaining lever is this variable.
// (claude v2.1.220: s3() → `!ENABLE_TOOL_SEARCH && provider==="firstParty" &&
// !Yd()`; a falsy value short-circuits via WKr()==="standard".)
const toolSearchEnv = "ENABLE_TOOL_SEARCH"

// mcpTokenEnv is the variable Claude carries the proxy's own bearer as, for
// the MCP agent-control server's Authorization header. It must be in
// Managed: without stripping, a nested launch from inside a proxied session
// would adopt the outer session's MCP token, exactly as ANTHROPIC_BASE_URL
// would without the same treatment.
const mcpTokenEnv = "RAFIKI_MCP_TOKEN"

// mcpServerName is the JSON key under mcpServers, and — by Claude Code's own
// convention — the prefix every tool it exposes appears under
// (mcp__rafiki__agent_spawn, etc).
const mcpServerName = "rafiki"

// mcpPath is the MCP agent-control surface's mount path. It cannot be
// imported from cmd/rafikid/mcp_face.go's mcpFacePath constant: proxyenv is
// a leaf package used by both the rafiki and rafikid binaries, and
// cmd/rafikid is a main package neither may import. Grep the other side
// before changing either.
const mcpPath = "/mcp"

// AnthropicModel reports whether model is served by Anthropic, and so can use
// the deferred tools Claude Code would otherwise send to a model that cannot
// call them. `rafiki claude` also uses it to refuse --passthrough-auth against
// a model a Claude subscription cannot buy, before the session starts.
//
// This is a shape test rather than a catalog lookup on purpose: proxyenv is a
// leaf package, the answer is needed before any network is available, and
// being wrong in the "not Anthropic" direction only costs a fatter tools[]
// while being wrong the other way costs the whole session.
//
// Note it is deliberately no substitute for the proxy's own check. A bare
// alias resolves through routing.ResolveModel's table (glm-5.2 → an OpenRouter
// id), and "~anthropic/..." keeps its tilde and routes to OpenRouter — this
// predicate calls the first not-Anthropic and the second Anthropic. It is a
// launch-time courtesy that errs toward letting work through; the proxy's 400
// remains the authority.
func AnthropicModel(model string) bool {
	if model == "" {
		return true // no override: Claude Code picks one of its own Anthropic ids
	}
	m := strings.ToLower(strings.TrimPrefix(model, "~"))
	m = strings.TrimPrefix(m, "anthropic/")
	if strings.Contains(m, "/") {
		return false // another provider's OpenRouter slash id
	}
	// A bare "<family>-latest" alias (opus-latest, fable-latest) is Anthropic's;
	// every other provider is reachable only through a slash id, caught above.
	return strings.HasPrefix(m, "claude") || strings.HasSuffix(m, "-latest")
}

// ClaudeOptions describes one proxied Claude Code session.
type ClaudeOptions struct {
	// URL is the rafiki base URL. Empty means "not proxied": Claude sets
	// nothing and strips nothing, so the caller gets the unmodified
	// environment back and talks to Anthropic directly.
	URL string
	// Token is the proxy's bearer. Claude Code requires a non-empty auth token
	// to send anything to a custom base URL at all, so an empty one is
	// replaced with a placeholder rather than omitted.
	Token string
	// MCPToken is the credential the injected MCP config authenticates with.
	// Empty falls back to Token, which is what keeps the interactive path (where
	// the two are the same user token) unchanged. The child paths set it
	// separately because their proxy bearer must stay the per-boot secret for
	// billing attribution while their MCP credential identifies one child.
	MCPToken string
	// PassthroughAuth leaves the client's own upstream credential in charge.
	// ANTHROPIC_AUTH_TOKEN is not set at all — its presence is exactly what
	// makes Claude Code choose API-key auth over its OAuth subscription — and
	// rafiki's own token moves to the X-Rafiki-Token header, where the proxy
	// reads it without colliding with the caller's Authorization.
	PassthroughAuth bool
	// Model may be empty, leaving Claude Code's own default in place.
	Model string
	// AutoCompactWindow of 0 leaves Claude Code's default threshold alone.
	AutoCompactWindow int
	// Headers are sent via ANTHROPIC_CUSTOM_HEADERS. Order is normalised so a
	// respawn with the same inputs produces the same environment.
	Headers map[string]string
}

// mcpServerConfig is one entry of a Claude Code --mcp-config document's
// "mcpServers" map, for the "http" transport.
type mcpServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// mcpConfigJSON returns the bare inline MCP-server JSON document that wires
// the rafiki agent-control MCP surface into a Claude Code session pointed at
// baseURL. It is the shape claudeargv.Params.MCPConfig expects — Build itself
// prepends the "--mcp-config=" prefix — so rendering the flag belongs to the
// argv producer (Claude below), never to this value: a caller that feeds the
// returned string to Params verbatim must get one --mcp-config element, not a
// doubled one. The token travels as a placeholder Claude Code expands from the
// RAFIKI_MCP_TOKEN environment variable at connect time, never inline —
// argv is world-readable via ps on this machine.
func mcpConfigJSON(baseURL string) string {
	doc := struct {
		MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	}{
		MCPServers: map[string]mcpServerConfig{
			mcpServerName: {
				Type: "http",
				URL:  baseURL + mcpPath,
				Headers: map[string]string{
					"Authorization": "Bearer ${" + mcpTokenEnv + "}",
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		// doc's only variable content is baseURL, an ordinary string;
		// json.Marshal cannot fail on this shape.
		panic("proxyenv: marshaling mcp config: " + err.Error())
	}
	return string(b)
}

// Values are the argv-shaped decisions Claude makes, returned as data so the
// caller can put them in claudeargv.Params instead of appending pre-rendered
// argv. Returning argv is what let two producers emit --model and forced
// daraja to reconcile them by scanning argv at launch.
type Values struct {
	// MCPConfig is the bare inline MCP-server JSON for
	// claudeargv.Params.MCPConfig, which itself prepends the --mcp-config=
	// prefix when it renders argv. This is the bare document, NOT the rendered
	// element — every consumer feeds it to Params verbatim, and a prefix here
	// would come out of Build doubled (--mcp-config=--mcp-config={...}). Empty
	// when no proxy URL is configured.
	MCPConfig string
	// ModelArgs is the custom-model-option --model pair for
	// claudeargv.Params.ModelArgs, or nil when no model override applies.
	ModelArgs []string
}

// Claude returns a complete environment derived from environ with the proxy
// wired in, plus the arguments to pass to the claude binary.
//
// The returned environment is complete rather than a set of additions, because
// stripping is half the job and you cannot un-set a variable by appending to a
// list.
func Claude(environ []string, o ClaudeOptions) (env []string, args []string) {
	env, v := ClaudeEnv(environ, o)
	if v.MCPConfig != "" {
		// The prefix is rendered HERE, at the argv producer, against the bare
		// JSON Values.MCPConfig carries — mirroring claudeargv.Build, which
		// prepends the same prefix to Params.MCPConfig. The rendered element
		// is byte-identical to the mcpConfigArg element this wrapper used to
		// append.
		args = append(args, "--mcp-config="+v.MCPConfig)
	}
	args = append(args, v.ModelArgs...)
	return env, args
}

// ClaudeEnv returns the same environment Claude returns, plus its argv
// decisions as data rather than as argv.
func ClaudeEnv(environ []string, o ClaudeOptions) ([]string, Values) {
	if o.URL == "" {
		return slices.Clone(environ), Values{} // not proxied: leave everything alone
	}

	env := make([]string, 0, len(environ)+8)
	present := make(map[string]bool, len(environ))
	for _, e := range environ {
		k, _, _ := strings.Cut(e, "=")
		if slices.Contains(Managed, k) || slices.Contains(Credentials, k) {
			continue
		}
		present[k] = true
		env = append(env, e)
	}

	env = append(env, "ANTHROPIC_BASE_URL="+o.URL)
	// The MCP agent-control surface's token travels by environment, never
	// inline in argv: argv is world-readable via ps on this machine, and
	// --mcp-config below carries only a placeholder Claude Code expands.
	mcpToken := o.Token
	if o.MCPToken != "" {
		mcpToken = o.MCPToken
	}
	env = append(env, mcpTokenEnv+"="+mcpToken)
	var v Values
	v.MCPConfig = mcpConfigJSON(o.URL)
	if !o.PassthroughAuth {
		token := o.Token
		if token == "" {
			// Claude Code will not send to a custom base URL without one; a proxy
			// that authenticates by other means (tailnet identity, anonymous dev)
			// still needs the field populated. Under PassthroughAuth the opposite
			// is true: the variable must be absent so Claude Code's own OAuth
			// credential takes over, and the placeholder would defeat the feature.
			token = "rafiki"
		}
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+token)
	}
	for _, d := range Defaults {
		k, _, _ := strings.Cut(d, "=")
		if !present[k] {
			env = append(env, d)
		}
	}

	if !present[toolSearchEnv] && !AnthropicModel(o.Model) {
		env = append(env, toolSearchEnv+"=false")
	}

	headers := o.Headers
	if o.PassthroughAuth && o.Token != "" {
		// Copy rather than mutate: the caller owns this map and may reuse it.
		headers = make(map[string]string, len(o.Headers)+1)
		maps.Copy(headers, o.Headers)
		headers["X-Rafiki-Token"] = o.Token
	}
	if h := FormatHeaders(headers); h != "" {
		env = append(env, "ANTHROPIC_CUSTOM_HEADERS="+h)
	}

	if o.Model != "" {
		// Register the model as a custom /model option rather than setting
		// ANTHROPIC_MODEL or passing a bare --model. Claude Code validates
		// those against a client-side allowlist of Anthropic ids and rejects
		// anything else BEFORE a request leaves, which makes every OpenRouter
		// slash id and every <family>-latest alias unreachable. A registered
		// custom option is sent verbatim, leaving resolution to the proxy —
		// the only side that can do it.
		env = append(env,
			"ANTHROPIC_CUSTOM_MODEL_OPTION="+o.Model,
			"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=rafiki: "+o.Model,
		)
		v.ModelArgs = []string{"--model", o.Model}
		if o.AutoCompactWindow > 0 {
			// Claude Code assumes a 200K context for a proxied model it cannot
			// verify, so it compacts at the wrong point for the real window.
			env = append(env, fmt.Sprintf("CLAUDE_CODE_AUTO_COMPACT_WINDOW=%d", o.AutoCompactWindow))
		}
	}
	return env, v
}

// FormatHeaders renders headers for ANTHROPIC_CUSTOM_HEADERS.
//
// The separator is a literal newline and nothing else. A comma, a semicolon or
// an escaped "\n" each silently collapse into ONE malformed header — no error,
// the correlation simply stops working. That single fact is why the daemon
// needs an environment file at all: systemd's Environment= is line-based and
// cannot carry this value (see paths.ServiceEnvFile).
//
// Keys are emitted in sorted order so the same inputs always produce the same
// string; an environment that differs run to run is needless noise when
// diffing what a child was actually given.
func FormatHeaders(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// A newline in a value would forge an extra header; a header whose
		// value contains one is malformed anyway, so drop it rather than emit
		// something the proxy would read as two.
		if strings.ContainsAny(h[k], "\n\r") {
			continue
		}
		parts = append(parts, k+": "+h[k])
	}
	return strings.Join(parts, "\n")
}
