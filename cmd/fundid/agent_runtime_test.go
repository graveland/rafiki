package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestAgentRunnerKind proves agentRunner only builds an in-process Runner for
// Kind: "fundi" and leaves every other kind on the subprocess path (nil
// Runner, nil error) unchanged.
func TestAgentRunnerKind(t *testing.T) {
	c := newTestController(t)
	for _, kind := range []string{protocol.KindPi, protocol.KindClaude} {
		req := protocol.SpawnRequest{Kind: kind, Cwd: t.TempDir()}
		runner, err := c.agentRunner(req, "c_"+kind)
		if err != nil {
			t.Fatalf("agentRunner(kind=%s): %v", kind, err)
		}
		if runner != nil {
			t.Errorf("agentRunner(kind=%s) returned a non-nil Runner, want nil", kind)
		}
	}

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5"}
	runner, err := c.agentRunner(req, "c_agent")
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
	ro, err := c.agentRuntimeOptions(req, "c_authoritative")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Ref != "c_authoritative" {
		t.Errorf("Ref = %q, want the daemon's child id to win over a competing ExtraArgs --ref", ro.Ref)
	}
}

// TestAgentRunnerAPIKeyOverlay proves req.APIKey reaches RuntimeOptions for an
// in-process child. buildEnv is how the subprocess path delivers this (via
// ANTHROPIC_API_KEY/OPENROUTER_API_KEY env injection), which an in-process
// child never receives since child.Spawn skips spec.Env when spec.Runner !=
// nil - so agentRuntimeOptions must overlay it directly onto RuntimeOptions,
// or it is silently dropped.
func TestAgentRunnerAPIKeyOverlay(t *testing.T) {
	// toRuntimeOptions seeds both key fields from os.Getenv, so the "other
	// field is empty" assertions below are only meaningful with a controlled,
	// empty ambient environment - on a dev machine whose shell/.env exports
	// both ANTHROPIC_API_KEY and OPENROUTER_API_KEY this test fails without
	// this, and in a clean CI it passes vacuously (on absence, not behavior).
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	c := newTestController(t)

	anthropicReq := protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    t.TempDir(),
		Model:  "anthropic/claude-sonnet-4-5",
		APIKey: "sk-ant-test-key",
	}
	ro, err := c.agentRuntimeOptions(anthropicReq, "c_key_anthropic")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.AnthropicAPIKey != anthropicReq.APIKey {
		t.Errorf("AnthropicAPIKey = %q, want %q", ro.AnthropicAPIKey, anthropicReq.APIKey)
	}
	if ro.OpenRouterAPIKey != "" {
		t.Errorf("OpenRouterAPIKey = %q, want empty for an anthropic/ model", ro.OpenRouterAPIKey)
	}

	openrouterReq := protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    t.TempDir(),
		Model:  "deepseek/deepseek-chat",
		APIKey: "sk-or-test-key",
	}
	ro, err = c.agentRuntimeOptions(openrouterReq, "c_key_openrouter")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.OpenRouterAPIKey != openrouterReq.APIKey {
		t.Errorf("OpenRouterAPIKey = %q, want %q", ro.OpenRouterAPIKey, openrouterReq.APIKey)
	}
	if ro.AnthropicAPIKey != "" {
		t.Errorf("AnthropicAPIKey = %q, want empty for a non-anthropic model", ro.AnthropicAPIKey)
	}
}

// TestAgentRunnerAPIKeyOverlayWinsOverExtraArgsModel is the assertion that
// actually exercises the Critical fix's "keyed off f.model, not req.Model"
// requirement: without it (i.e. keying off req.Model instead), req.Model and
// the ExtraArgs --model override agree in every other test in this file, so
// reverting to req.Model would leave the rest of the suite green.
func TestAgentRunnerAPIKeyOverlayWinsOverExtraArgsModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	c := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/x",
		ExtraArgs: []string{"--model", "deepseek/y"},
		APIKey:    "sk-test-key",
	}
	ro, err := c.agentRuntimeOptions(req, "c_model_override_key")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.OpenRouterAPIKey != req.APIKey {
		t.Errorf("OpenRouterAPIKey = %q, want %q (the ExtraArgs --model override should route the key here, not AnthropicAPIKey)", ro.OpenRouterAPIKey, req.APIKey)
	}
	if ro.AnthropicAPIKey != "" {
		t.Errorf("AnthropicAPIKey = %q, want empty: keying off req.Model instead of the parsed f.model would wrongly land the key here", ro.AnthropicAPIKey)
	}
}

// TestAgentRunnerEnvOverlay proves the two names an in-process child can
// actually receive from req.Env (ANTHROPIC_API_KEY/OPENROUTER_API_KEY) reach
// RuntimeOptions, that req.Env wins over the ambient daemon environment, and
// that an explicit req.APIKey still wins over a forwarded req.Env value
// (daemon env < forwarded env < explicit key).
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
	ro, err := c.agentRuntimeOptions(req, "c_env_overlay")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.AnthropicAPIKey != "forwarded-anthropic" {
		t.Errorf("AnthropicAPIKey = %q, want the forwarded req.Env value to win over the ambient daemon env", ro.AnthropicAPIKey)
	}
	if ro.OpenRouterAPIKey != "ambient-openrouter" {
		t.Errorf("OpenRouterAPIKey = %q, want the ambient daemon env preserved (req.Env didn't touch this key)", ro.OpenRouterAPIKey)
	}

	// An explicit req.APIKey must still win over a forwarded req.Env value.
	req.APIKey = "explicit-key"
	ro, err = c.agentRuntimeOptions(req, "c_env_overlay_explicit_wins")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.AnthropicAPIKey != "explicit-key" {
		t.Errorf("AnthropicAPIKey = %q, want the explicit req.APIKey to win over both ambient and forwarded env", ro.AnthropicAPIKey)
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
		if _, err := c.agentRuntimeOptions(req, "c_explicit_db"); err == nil {
			t.Errorf("agentRuntimeOptions(ExtraArgs=%v): want an error rejecting explicit --db, got nil", extraArgs)
		}
	}
}

// TestAgentRunnerIgnoresEnvDefaultedDB proves the daemon's own
// $FUNDI_AGENT_DB (read as agentFlags.db's default by newAgentFlagSet, with
// zero ExtraArgs involved) is NOT treated as an explicit --db and does not
// trip the rejection above — it names the same database the shared pool
// already points at, so an ordinary deployment with a configured database
// must still be able to spawn agent children.
func TestAgentRunnerIgnoresEnvDefaultedDB(t *testing.T) {
	t.Setenv(paths.AgentDB, "postgres://daemons-own-pool")
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	if _, err := c.agentRuntimeOptions(req, "c_env_default_db"); err != nil {
		t.Errorf("agentRuntimeOptions with only $FUNDI_AGENT_DB set (no explicit --db): got error %v, want nil", err)
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

	got, err := f.toRuntimeOptions(req.Cwd, nil)
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
	got, err := f.toRuntimeOptions(req.Cwd, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Model != "deepseek/deepseek-chat" {
		t.Errorf("Model = %q, want the ExtraArgs override to win", got.Model)
	}
}

// TestAgentRefIsDaemonControlled proves the child id reaches the engine. It
// normally arrives via the injected FUNDI_CHILD_ID env var, which an in-process
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
	got, err := f.toRuntimeOptions(req.Cwd, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Ref != "c_authoritative" {
		t.Errorf("Ref = %q, want the daemon's child id to win over ExtraArgs", got.Ref)
	}
}
