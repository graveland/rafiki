package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
)

// TestDarajaClaudeParams_NoProxyConfiguredLeavesFieldsEmpty proves the
// unproxied case (no daemon proxy face, or claude not in RAFIKI_PROXY_KINDS)
// degrades to exactly the pre-Phase-2 ClaudeParams: no proxy fields set, so
// daraja's env stays untouched, matching before these fields existed.
func TestDarajaClaudeParams_NoProxyConfiguredLeavesFieldsEmpty(t *testing.T) {
	c := newTestController(t)
	// c.proxyURL is "" by default in newTestController — no proxy face wired.
	p := c.darajaClaudeParams(protocol.SpawnRequest{Kind: protocol.KindClaude, Model: "claude-sonnet-5"})
	if p.ProxyUrl != "" || p.ProxyToken != "" || p.PassthroughAuth {
		t.Errorf("darajaClaudeParams with no proxy configured = %+v, want no proxy fields set", p)
	}
	if p.Model != "claude-sonnet-5" || p.PermissionMode != "bypassPermissions" {
		t.Errorf("darajaClaudeParams = %+v, want Model/PermissionMode preserved unchanged", p)
	}
}

// TestDarajaClaudeParams_PassthroughTriState proves the auto/on/off resolution
// against model — the whole point of exposing this as a request field rather
// than a daemon-wide default.
func TestDarajaClaudeParams_PassthroughTriState(t *testing.T) {
	cases := []struct {
		name            string
		passthroughAuth string
		model           string
		want            bool
	}{
		{"auto + anthropic model bills subscription", "", "claude-sonnet-5", true},
		{"auto + non-anthropic model bills daemon key", "", "openai/gpt-4o", false},
		{"auto + no model bills subscription", "", "", true},
		{"explicit off overrides anthropic model", "off", "claude-sonnet-5", false},
		{"explicit on overrides non-anthropic model", "on", "openai/gpt-4o", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestController(t)
			c.proxyURL, c.proxyToken = "http://127.0.0.1:1/", "tok"
			p := c.darajaClaudeParams(protocol.SpawnRequest{
				Kind: protocol.KindClaude, Model: tc.model, PassthroughAuth: tc.passthroughAuth,
			})
			if p.PassthroughAuth != tc.want {
				t.Errorf("PassthroughAuth = %v, want %v", p.PassthroughAuth, tc.want)
			}
			if p.ProxyUrl != c.proxyURL || p.ProxyToken != c.proxyToken {
				t.Errorf("ProxyUrl/ProxyToken = %q/%q, want %q/%q", p.ProxyUrl, p.ProxyToken, c.proxyURL, c.proxyToken)
			}
		})
	}
}

// TestDarajaClaudeParams_RecordRequestsThreadsThrough is a narrow regression
// guard: a request-level opt-in silently dropped costs a debugging session
// with no error anywhere, the same failure class proxyChildEnv's own
// RecordRequests threading already had to get right once.
func TestDarajaClaudeParams_RecordRequestsThreadsThrough(t *testing.T) {
	c := newTestController(t)
	c.proxyURL = "http://127.0.0.1:1/"
	p := c.darajaClaudeParams(protocol.SpawnRequest{Kind: protocol.KindClaude, RecordRequests: true})
	if !p.RecordRequests {
		t.Error("RecordRequests did not thread through darajaClaudeParams")
	}
}

// TestAgentRunnerKind proves agentRunner only builds an in-process Runner for
// Kind: "fundi" and leaves every other kind on the subprocess path (nil
// Runner, nil error) unchanged.
func TestAgentRunnerKind(t *testing.T) {
	c := newTestController(t)
	for _, kind := range []string{protocol.KindClaude} {
		req := protocol.SpawnRequest{Kind: kind, Cwd: t.TempDir()}
		runner, err := c.agentRunner(req, "c_"+kind, false, "", "", nil)
		if err != nil {
			t.Fatalf("agentRunner(kind=%s): %v", kind, err)
		}
		if runner != nil {
			t.Errorf("agentRunner(kind=%s) returned a non-nil Runner, want nil", kind)
		}
	}

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5"}
	runner, err := c.agentRunner(req, "c_agent", false, "", "", nil)
	if err != nil {
		t.Fatalf("agentRunner(kind=agent): %v", err)
	}
	if runner == nil {
		t.Error("agentRunner(kind=agent) returned a nil Runner, want non-nil")
	}
}

// TestAgentRunnerRefWinsOverExtraArgs is the agentRunner-level counterpart to
// TestAgentRefIsDaemonControlled: it goes through the full agentRunner path
// (not just buildAgentArgv/parseAgentFlags/toRuntimeOptions directly), so it
// is the test that actually catches a dropped appendDaemonRef call inside
// agentRunner/agentRuntimeOptions itself — the earlier tests call
// appendDaemonRef directly and would keep passing even if agentRunner forgot
// to call it.
func TestAgentRunnerRefWinsOverExtraArgs(t *testing.T) {
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-sonnet-4-5",
		ExtraArgs: []string{"--ref", "spoofed-child-id"},
	}
	ro, err := c.agentRuntimeOptions(req, "c_authoritative", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Ref != "c_authoritative" {
		t.Errorf("Ref = %q, want the daemon's child id to win over a competing ExtraArgs --ref", ro.Ref)
	}
}

// TestAgentRuntimeOptionsQuotaNilOnDBLessDaemon guards against the classic Go
// nil-pointer-in-non-nil-interface trap: ro.Quota must be a true nil
// tools.QuotaReader (opts.Quota == nil) on a DB-less daemon, or quota_status
// would materialize into every spawn-capable agent's tools[] and permanently
// answer "no data captured" instead of declining outright, per its
// Materialize doc comment.
func TestAgentRuntimeOptionsQuotaNilOnDBLessDaemon(t *testing.T) {
	c := newTestController(t) // pool == nil
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_no_db", false, "brent", "user-id-123")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Quota != nil {
		t.Errorf("Quota = %#v, want nil on a DB-less daemon", ro.Quota)
	}
}

// TestAgentRunnerAPIKeyOverlay proves req.APIKey reaches RuntimeOptions
// as APIKeyOverride for an in-process child, regardless of which provider
// the model addresses.
func TestAgentRunnerAPIKeyOverlay(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	c := newTestController(t)

	anthropicReq := protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    t.TempDir(),
		Model:  "anthropic/claude-sonnet-4-5",
		APIKey: "sk-ant-test-key",
	}
	ro, err := c.agentRuntimeOptions(anthropicReq, "c_key_anthropic", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.APIKeyOverride != anthropicReq.APIKey {
		t.Errorf("APIKeyOverride = %q, want %q", ro.APIKeyOverride, anthropicReq.APIKey)
	}

	openrouterReq := protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    t.TempDir(),
		Model:  "openrouter/deepseek/deepseek-chat",
		APIKey: "sk-or-test-key",
	}
	ro, err = c.agentRuntimeOptions(openrouterReq, "c_key_openrouter", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.APIKeyOverride != openrouterReq.APIKey {
		t.Errorf("APIKeyOverride = %q, want %q", ro.APIKeyOverride, openrouterReq.APIKey)
	}
}

// TestAgentRunnerAPIKeyOverlayWinsOverExtraArgsModel verifies that the
// per-spawn key is forwarded as APIKeyOverride regardless of ExtraArgs --model.
func TestAgentRunnerAPIKeyOverlayWinsOverExtraArgsModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	c := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/x",
		ExtraArgs: []string{"--model", "openrouter/deepseek/y"},
		APIKey:    "sk-test-key",
	}
	ro, err := c.agentRuntimeOptions(req, "c_model_override_key", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.APIKeyOverride != req.APIKey {
		t.Errorf("APIKeyOverride = %q, want %q (the per-spawn key should always be forwarded)", ro.APIKeyOverride, req.APIKey)
	}
}

// TestAgentRunnerEnvOverlay proves that forwarded env vars reach
// RuntimeOptions.Env and that API keys are NOT placed in Env where os.Setenv
// would expose them. APIKeyOverride is carried separately. Also proves that
// req.APIKey wins over anything forwarded in env.
func TestAgentRunnerEnvOverlay(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ambient-anthropic")
	t.Setenv("OPENROUTER_API_KEY", "ambient-openrouter")
	c := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "forwarded-anthropic",
			"http_proxy":        "http://example.invalid:8080",
		},
	}
	ro, err := c.agentRuntimeOptions(req, "c_env_overlay", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	// API key env vars are NOT forwarded to Env
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if _, ok := ro.Env[key]; ok {
			t.Errorf("Env[%q] should be absent (API keys must not reach os.Setenv)", key)
		}
	}
	// Non-API env vars must reach Env.
	if got := ro.Env["http_proxy"]; got != "http://example.invalid:8080" {
		t.Errorf("Env[http_proxy] = %q, want http://example.invalid:8080", got)
	}

	// An explicit req.APIKey must be forwarded as APIKeyOverride.
	req.APIKey = "explicit-key"
	ro, err = c.agentRuntimeOptions(req, "c_env_overlay_explicit_wins", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.APIKeyOverride != "explicit-key" {
		t.Errorf("APIKeyOverride = %q, want the explicit req.APIKey to win", ro.APIKeyOverride)
	}
}

// TestAgentSpawnHasExplicitDBSingleDash proves the single-dash spellings
// flag.FlagSet also accepts (-db, -db=...) are detected, not just the
// double-dash forms - flag.NewFlagSet takes one or two leading dashes, so a
// detector that only matched "--db"/"--db=" let a single-dash caller's DSN
// through undetected, silently discarding it (the exact failure mode this
// check exists to eliminate).
func TestAgentSpawnHasExplicitDBSingleDash(t *testing.T) {
	for _, extraArgs := range [][]string{
		{"-db", "postgres://caller-supplied"},
		{"-db=postgres://caller-supplied"},
	} {
		if !agentSpawnHasExplicitDB(extraArgs) {
			t.Errorf("agentSpawnHasExplicitDB(%v) = false, want true", extraArgs)
		}
	}
}

// TestAgentRunnerRejectsExplicitDB proves an explicit --db in req.ExtraArgs is
// rejected rather than silently discarded: the in-process path always uses
// the daemon's own shared pool (c.pool), so honoring a caller's own DSN would
// require opening a second pool this code never does.
func TestAgentRunnerRejectsExplicitDB(t *testing.T) {
	c := newTestController(t)
	for _, extraArgs := range [][]string{
		{"--db", "postgres://caller-supplied"},
		{"--db=postgres://caller-supplied"},
	} {
		req := protocol.SpawnRequest{
			Kind:      protocol.KindFundi,
			Cwd:       t.TempDir(),
			Model:     "anthropic/claude-sonnet-4-5",
			ExtraArgs: extraArgs,
		}
		if _, err := c.agentRuntimeOptions(req, "c_explicit_db", false, "", ""); err == nil {
			t.Errorf("agentRuntimeOptions(ExtraArgs=%v): want an error rejecting explicit --db, got nil", extraArgs)
		}
	}
}

// TestAgentRunnerIgnoresEnvDefaultedDB proves the daemon's own
// $RAFIKI_DB (read as agentFlags.db's default by newAgentFlagSet, with
// zero ExtraArgs involved) is NOT treated as an explicit --db and does not
// trip the rejection above — it names the same database the shared pool
// already points at, so an ordinary deployment with a configured database
// must still be able to spawn agent children.
func TestAgentRunnerIgnoresEnvDefaultedDB(t *testing.T) {
	t.Setenv(paths.DB, "postgres://daemons-own-pool")
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	if _, err := c.agentRuntimeOptions(req, "c_env_default_db", false, "", ""); err != nil {
		t.Errorf("agentRuntimeOptions with only $RAFIKI_DB set (no explicit --db): got error %v, want nil", err)
	}
}

// TestArgvRoundTripsIntoRuntimeOptions is the anti-drop guard for this task.
// buildAgentArgv is the single place per-child config is expressed; parsing it
// back must reproduce every value. A field that buildAgentArgv emits and
// toRuntimeOptions ignores is silently lost for every in-process child, which
// is precisely how Resume lost SkillsDirs and MCPConfig while all tests passed.
func TestArgvRoundTripsIntoRuntimeOptions(t *testing.T) {
	mcp := t.TempDir() + "/mcp.json"
	req := protocol.SpawnRequest{
		Kind:               protocol.KindFundi,
		Cwd:                t.TempDir(),
		Model:              "anthropic/claude-sonnet-4-5",
		Thinking:           "high",
		SystemPrompt:       "SYSTEM-MARKER",
		AppendSystemPrompt: "APPEND-MARKER",
		Skills:             []string{"alpha", "beta"},
		Name:               "NAME-MARKER",
		SkillsDirs:         []string{"/tmp/skills-one", "/tmp/skills-two"},
		MCPConfig:          mcp,
	}

	argv := buildAgentArgv(req, "c_round", t.TempDir())
	if argv[0] != protocol.KindFundi {
		t.Fatalf("argv[0] = %q, want %q", argv[0], protocol.KindFundi)
	}

	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags(%q): %v", argv[1:], err)
	}

	got, err := f.toRuntimeOptions(req.Cwd, nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}

	if got.Model != req.Model {
		t.Errorf("Model = %q, want %q", got.Model, req.Model)
	}
	if got.SystemPromptOverride != req.SystemPrompt {
		t.Errorf("SystemPromptOverride = %q, want %q", got.SystemPromptOverride, req.SystemPrompt)
	}
	if got.AppendSystemPrompt != req.AppendSystemPrompt {
		t.Errorf("AppendSystemPrompt = %q, want %q", got.AppendSystemPrompt, req.AppendSystemPrompt)
	}
	if got.Skills != "alpha,beta" {
		t.Errorf("Skills = %q, want \"alpha,beta\"", got.Skills)
	}
	if got.Name != req.Name {
		t.Errorf("Name = %q, want %q", got.Name, req.Name)
	}
	if got.MCPConfig != mcp {
		t.Errorf("MCPConfig = %q, want %q", got.MCPConfig, mcp)
	}
	if got.SpillDir == "" {
		t.Error("SpillDir is empty; buildAgentArgv always passes --spill-dir")
	}
	if got.ThinkingBudget == 0 {
		t.Error("ThinkingBudget = 0 for --thinking high; the conversion was skipped")
	}
	// SkillsDirs must include both --skills-dir values. assembleSkillDirs
	// prepends the configured and per-project dirs, so assert containment.
	joined := strings.Join(got.SkillsDirs, ":")
	for _, want := range req.SkillsDirs {
		if !strings.Contains(joined, want) {
			t.Errorf("SkillsDirs %v missing %q", got.SkillsDirs, want)
		}
	}
}

// TestExtraArgsOverrideEarlierFlags proves the last-flag-wins convention
// buildAgentArgv documents survives the in-process path. ExtraArgs are appended
// last precisely so a caller can override, and an in-process child that ignored
// them would diverge from a subprocess one.
func TestExtraArgsOverrideEarlierFlags(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-sonnet-4-5",
		ExtraArgs: []string{"--model", "deepseek/deepseek-chat"},
	}
	argv := buildAgentArgv(req, "c_extra", t.TempDir())
	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	got, err := f.toRuntimeOptions(req.Cwd, nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Model != "deepseek/deepseek-chat" {
		t.Errorf("Model = %q, want the ExtraArgs override to win", got.Model)
	}
}

// TestAgentRefIsDaemonControlled proves the child id reaches the engine. It
// normally arrives via the injected RAFIKI_CHILD_ID env var, which an in-process
// child never inherits, so it must be appended to argv after ExtraArgs — a
// caller must not be able to point one child at another's conversation.
func TestAgentRefIsDaemonControlled(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-sonnet-4-5",
		ExtraArgs: []string{"--ref", "spoofed-child-id"},
	}
	argv := appendDaemonRef(buildAgentArgv(req, "c_authoritative", t.TempDir()), "c_authoritative")
	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	got, err := f.toRuntimeOptions(req.Cwd, nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Ref != "c_authoritative" {
		t.Errorf("Ref = %q, want the daemon's child id to win over ExtraArgs", got.Ref)
	}
}

// TestToRuntimeOptionsUsesSharedLSPAndToolsWebResolution is the behavioral
// half of finding 15's regression guard (the structural half is
// TestLSPAndToolsWebHelpersSharedAcrossCallSites in agent_test.go): for a
// matrix of --lsp-config/--tools-web/$RAFIKI_TOOLS_WEB combinations, it
// proves toRuntimeOptions' LSPConfig and ToolsWeb fields equal what calling
// effectiveLSPConfig/toolsWebValue directly (the exact calls runAgent also
// makes, in agent.go) produces for the identical inputs. Before the
// extraction, runAgent and toRuntimeOptions each hand-rolled this precedence
// with no shared helper, so nothing forced the two to compute the same
// answer on a given input; this test would have failed the moment either
// copy drifted.
func TestToRuntimeOptionsUsesSharedLSPAndToolsWebResolution(t *testing.T) {
	cwdWithLSP := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwdWithLSP, ".lsp.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cwdWithoutLSP := t.TempDir()

	cases := []struct {
		name        string
		lspConfig   string
		cwd         string
		toolsWeb    bool
		toolsWebSet bool
		envWeb      string
	}{
		{"defaulted lsp present, tools-web unset", "", cwdWithLSP, false, false, ""},
		{"defaulted lsp absent, tools-web unset", "", cwdWithoutLSP, false, false, ""},
		{"explicit lsp missing survives, tools-web flag on", "/nope/lsp.json", cwdWithoutLSP, true, true, ""},
		{"tools-web flag off beats env on", "", cwdWithoutLSP, false, true, "1"},
		{"tools-web from env only", "", cwdWithoutLSP, false, false, "1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RAFIKI_TOOLS_WEB", tc.envWeb)

			f := agentFlags{
				model:       "anthropic/claude-sonnet-4-5",
				lspConfig:   tc.lspConfig,
				toolsWeb:    tc.toolsWeb,
				toolsWebSet: tc.toolsWebSet,
			}
			ro, err := f.toRuntimeOptions(tc.cwd, nil, false, nil)
			if err != nil {
				t.Fatalf("toRuntimeOptions: %v", err)
			}

			wantLSP := effectiveLSPConfig(tc.lspConfig, tc.cwd)
			if ro.LSPConfig != wantLSP {
				t.Errorf("toRuntimeOptions LSPConfig = %q, want %q (effectiveLSPConfig directly)", ro.LSPConfig, wantLSP)
			}

			wantWeb := toolsWebValue(tc.toolsWeb, tc.toolsWebSet)
			if ro.ToolsWeb != wantWeb {
				t.Errorf("toRuntimeOptions ToolsWeb = %v, want %v (toolsWebValue directly)", ro.ToolsWeb, wantWeb)
			}
		})
	}
}

func TestToRuntimeOptions_ModelDefaultAppliesWhenCallerSpecifiesNothing(t *testing.T) {
	prov, err := providers.Parse([]byte(modelDefaultsTOML)) // reuses the fixture from Task 2's model_defaults_test.go
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := agentFlags{model: "vmlx/qwen"}
	ro, err := f.toRuntimeOptions(t.TempDir(), nil, false, prov)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if !ro.NoSkills {
		t.Error("expected NoSkills=true: vmlx/qwen declares skills=\"\"")
	}
	if ro.MCPServers != "codescan" || ro.NoMCP {
		t.Errorf("MCPServers=%q NoMCP=%v, want MCPServers=\"codescan\" NoMCP=false", ro.MCPServers, ro.NoMCP)
	}
	if ro.ContextFilesBudget != 3276 {
		t.Errorf("ContextFilesBudget = %d, want 3276", ro.ContextFilesBudget)
	}
}

func TestToRuntimeOptions_ExplicitCallerValueWinsOverModelDefault(t *testing.T) {
	prov, err := providers.Parse([]byte(modelDefaultsTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := agentFlags{model: "vmlx/qwen", skills: "*", mcpServers: "other-only"}
	ro, err := f.toRuntimeOptions(t.TempDir(), nil, false, prov)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if ro.Skills != "*" || ro.NoSkills {
		t.Errorf("Skills=%q NoSkills=%v, want Skills=\"*\" NoSkills=false (caller override must win over the model's skills=\"\" default)", ro.Skills, ro.NoSkills)
	}
	if ro.MCPServers != "other-only" {
		t.Errorf("MCPServers = %q, want the caller's explicit \"other-only\"", ro.MCPServers)
	}
}

func TestToRuntimeOptions_NoAliasLeavesEverythingAtCallerValue(t *testing.T) {
	prov, err := providers.Parse([]byte(modelDefaultsTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := agentFlags{model: "anthropic/claude-sonnet-5"} // no alias declared for this model
	ro, err := f.toRuntimeOptions(t.TempDir(), nil, false, prov)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if ro.NoSkills || ro.Skills != "" {
		t.Errorf("Skills=%q NoSkills=%v, want the untouched zero value", ro.Skills, ro.NoSkills)
	}
	if ro.ContextFilesBudget != 0 {
		t.Errorf("ContextFilesBudget = %d, want 0 (no alias, no formula input)", ro.ContextFilesBudget)
	}
}

func baseRequest() protocol.SpawnRequest {
	return protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   "/tmp",
		Model: "anthropic/claude-sonnet-4-5",
	}
}

// A human typing --executor-selector gets explainNoMatch's per-candidate
// diagnostic immediately. Downgrading this to a slog.Warn produced a running,
// billing agent whose every workspace tool errored, with nothing surfaced.
func TestTopLevelSpawnWithAnUnmatchedSelectorIsRefused(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	req := baseRequest()
	req.ExecutorSelector = "env=nowhere"
	_, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err == nil {
		t.Fatal("a top-level spawn whose selector matches nothing must be refused")
	}
	if !strings.Contains(err.Error(), "env=nowhere") {
		t.Fatalf("the refusal must carry explainNoMatch's diagnostic, got: %v", err)
	}
}

// A child spawned by an agent may start unbound: an executor restart parks its
// connection for up to a full health tick, and surviving that window is what
// lazy binding is for.
func TestParentedSpawnWithNoLiveExecutorStartsUnbound(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	seedChild(t, c)
	req := baseRequest()
	req.ParentChildID = "c_parent"
	req.ExecutorSelector = "env=nowhere"
	ro, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err != nil {
		t.Fatalf("a parented spawn must be allowed to start unbound: %v", err)
	}
	if ro.Executor == nil {
		t.Fatal("Executor must still be non-nil, or MaterializeAll drops the " +
			"whole workspace tier and the child silently runs tools in the daemon")
	}
}

// An auto-resumed child (e.g. recovering on daemon restart before executors
// reconnect) is allowed to start unbound: its workspace tools will lazy-bind
// once the matching executor connects.
func TestAutoResumeWithNoLiveExecutorStartsUnbound(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	req := baseRequest()
	req.ExecutorSelector = "env=nowhere"
	ro, err := c.agentRuntimeOptions(req, "c1", true, "brent", "")
	if err != nil {
		t.Fatalf("an auto-resumed spawn must be allowed to start unbound: %v", err)
	}
	if ro.Executor == nil {
		t.Fatal("Executor must still be non-nil, or MaterializeAll drops the " +
			"whole workspace tier and the child silently runs tools in the daemon")
	}
}

// markUnbound writes "unbound" through the store-then-stash path so Spawn
// picks it up and stores it on initialization labels.
func TestAnUnboundChildSaysSoInItsLabels(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	seedChild(t, c)
	req := baseRequest()
	req.ParentChildID = "c_parent"
	req.ExecutorSelector = "env=nowhere"
	_, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err != nil {
		t.Fatal(err)
	}
	wl, ok := c.takeWorkspaceLabels("c1")
	if !ok {
		t.Fatal("markUnbound must stash the 'unbound' state for Spawn to pick up")
	}
	if wl.executorState != "unbound" {
		t.Fatalf("executorState = %q, want %q", wl.executorState, "unbound")
	}
}

// When a live executor admits the child's selector, the workspace block is
// built from the bound executor's row.
func TestABoundChildGetsTheWorkspaceBlock(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{
		live: []execpool.LiveExecutor{{
			Executor: executors.Executor{
				ID:      "exec-1",
				Enabled: true,
				Labels:  map[string]string{"env": "home"},
				Roots:   []string{"/tmp"},
			},
		}},
	}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Status: protocol.StatusIdle,
		Kind: protocol.KindFundi,
	})
	req := baseRequest()
	req.ParentChildID = "c_parent"
	req.ExecutorSelector = "env=home"
	ro, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err != nil {
		t.Fatal(err)
	}
	if ro.Workspace == nil {
		t.Fatal("a child that binds at spawn must get the workspace block; the " +
			"system prompt is fixed for its lifetime")
	}
	if ro.Workspace.Roots == nil || ro.Workspace.Roots[0] != "/tmp" {
		t.Fatalf("workspace block must carry the executor's roots, got %+v", ro.Workspace)
	}
}

// When no executor matches (not even as a candidate), the workspace block
// is nil. This is acceptable per the design: naming the wrong machine is
// worse than none.
func TestUnboundChildWithNoCandidatesGetsNilWorkspace(t *testing.T) {
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	seedChild(t, c)
	req := baseRequest()
	req.ParentChildID = "c_parent"
	req.ExecutorSelector = "env=nowhere"
	ro, err := c.agentRuntimeOptions(req, "c1", false, "brent", "")
	if err != nil {
		t.Fatal(err)
	}
	if ro.Workspace != nil {
		t.Fatal("with no live candidate, workspace block must be nil; a block " +
			"naming the wrong machine is worse than none")
	}
}

// TestAgentRuntimeOptionsWiresOnConsumed proves ro.OnConsumed is not merely
// non-nil but actually reaches Controller.consumeFrames and retires the right
// rows: without this wiring every delivered message stays 'sent' forever and
// is redelivered on the next daemon restart.
//
// A bare "ro.OnConsumed != nil" check would pass against a closure that does
// nothing. This seeds a real inbox row, records a sentFrame mapping for it
// exactly as deliverInbox does, invokes the callback with that frame id, and
// asserts the row became terminal (no longer pending) and the frame's
// bookkeeping was forgotten.
func TestAgentRuntimeOptionsWiresOnConsumed(t *testing.T) {
	c := newTestController(t)
	mem := inbox.NewMemory()
	c.inbox = c.newInboxQueue(mem)

	req := baseRequest()
	ro, err := c.agentRuntimeOptions(req, "c_1", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.OnConsumed == nil {
		t.Fatal("OnConsumed must be wired: without it every delivered message stays 'sent' forever")
	}

	ctx := context.Background()
	rec, err := mem.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := mem.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	c.sentMu.Lock()
	c.sentFrames["F1"] = sentFrame{childID: "c_1", rowIDs: []string{rec.ID}}
	c.sentMu.Unlock()

	ro.OnConsumed([]string{"F1"})

	if rows, err := mem.Pending(ctx, "c_1"); err != nil || len(rows) != 0 {
		t.Fatalf("the acked row should be terminal, still pending: %+v (err=%v)", rows, err)
	}
	c.sentMu.Lock()
	_, still := c.sentFrames["F1"]
	c.sentMu.Unlock()
	if still {
		t.Error("an acked frame must be forgotten")
	}
}

func TestAgentRunnerGrantsMaxCostToTheEngine(t *testing.T) {
	c := newTestController(t)
	budget := 12.50
	req := protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     t.TempDir(),
		Model:   "anthropic/claude-sonnet-4-5",
		MaxCost: &budget,
	}
	ro, err := c.agentRuntimeOptions(req, "c_budgeted", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.MaxCost != 12.50 {
		t.Errorf("ro.MaxCost = %v, want 12.50", ro.MaxCost)
	}
}

func TestAgentRunnerUnsetMaxCostIsUnlimited(t *testing.T) {
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_unbudgeted", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.MaxCost != 0 {
		t.Errorf("ro.MaxCost = %v, want 0 (unlimited)", ro.MaxCost)
	}
}
