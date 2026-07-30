package main

import (
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/protocol"
)

// TestArgvRoundTripsIntoRuntimeOptions is the anti-drop guard for this task.
// buildAgentArgv is the single place per-child config is expressed; parsing it
// back must reproduce every value. A field that buildAgentArgv emits and
// toRuntimeOptions ignores is silently lost for every in-process child, which
// is precisely how Resume lost SkillsDirs and MCPConfig while all tests passed.
func TestArgvRoundTripsIntoRuntimeOptions(t *testing.T) {
	mcp := t.TempDir() + "/mcp.json"
	req := protocol.SpawnRequest{
		Kind:               "agent",
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
	if argv[0] != "agent" {
		t.Fatalf("argv[0] = %q, want \"agent\"", argv[0])
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
		Kind:      "agent",
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
		Kind:      "agent",
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
