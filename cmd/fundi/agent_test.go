package main

import (
	"testing"

	"git.graveland.dev/brent/fundi/internal/agent"
)

func TestParseAgentFlagsDefaults(t *testing.T) {
	f, err := parseAgentFlags(nil)
	if err != nil {
		t.Fatalf("parseAgentFlags(nil): %v", err)
	}
	if f.model != "sonnet-latest" {
		t.Errorf("model = %q, want sonnet-latest", f.model)
	}
	if f.provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic (heuristic default for sonnet-latest)", f.provider)
	}
	if f.thinking != "off" {
		t.Errorf("thinking = %q, want off", f.thinking)
	}
}

func TestParseAgentFlagsProviderHeuristic(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "meta-llama/llama-3.1-70b"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.provider != "openrouter" {
		t.Errorf("provider = %q, want openrouter for a slash-containing model id", f.provider)
	}
}

func TestParseAgentFlagsExplicitProviderWins(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "meta-llama/llama-3.1-70b", "--provider", "anthropic"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic (explicit --provider overrides the heuristic)", f.provider)
	}
}

func TestParseAgentFlagsRefFromEnv(t *testing.T) {
	t.Setenv("PI_CONTROLLER_CHILD_ID", "child-123")
	f, err := parseAgentFlags(nil)
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "child-123" {
		t.Errorf("ref = %q, want child-123 (from $PI_CONTROLLER_CHILD_ID)", f.ref)
	}
}

func TestParseAgentFlagsRefExplicitFlagWins(t *testing.T) {
	t.Setenv("PI_CONTROLLER_CHILD_ID", "child-123")
	f, err := parseAgentFlags([]string{"--ref", "explicit-ref"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "explicit-ref" {
		t.Errorf("ref = %q, want explicit-ref (explicit --ref overrides the env default)", f.ref)
	}
}

func TestParseAgentFlagsDBFromEnv(t *testing.T) {
	t.Setenv("FUNDI_AGENT_DB", "postgres://example/db")
	f, err := parseAgentFlags(nil)
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.db != "postgres://example/db" {
		t.Errorf("db = %q, want postgres://example/db (from $FUNDI_AGENT_DB)", f.db)
	}
}

func TestParseAgentFlagsRepeatableSkillsDir(t *testing.T) {
	f, err := parseAgentFlags([]string{"--skills-dir", "/a", "--skills-dir", "/b"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if len(f.skillsDir) != 2 || f.skillsDir[0] != "/a" || f.skillsDir[1] != "/b" {
		t.Errorf("skillsDir = %v, want [/a /b]", f.skillsDir)
	}
}

func TestParseAgentFlagsThinkingLevel(t *testing.T) {
	f, err := parseAgentFlags([]string{"--thinking", "xhigh"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.thinking != "xhigh" {
		t.Errorf("thinking = %q, want xhigh", f.thinking)
	}
	if _, err := agent.ThinkingBudgetFor(f.thinking); err != nil {
		t.Errorf("ThinkingBudgetFor(%q): unexpected error: %v", f.thinking, err)
	}
}

func TestParseAgentFlagsRejectsUnknownFlag(t *testing.T) {
	if _, err := parseAgentFlags([]string{"--not-a-real-flag"}); err == nil {
		t.Fatal("parseAgentFlags with an unknown flag: want error, got nil")
	}
}

func TestParseAgentFlagsNoSkillsAndNoContextFiles(t *testing.T) {
	f, err := parseAgentFlags([]string{"--no-skills", "--no-context-files"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if !f.noSkills || !f.noContextFiles {
		t.Errorf("noSkills=%v noContextFiles=%v, want both true", f.noSkills, f.noContextFiles)
	}
}
