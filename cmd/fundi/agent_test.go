package main

import (
	"testing"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// TestParseAgentFlagsRequiresModel covers the redesign's central invariant:
// --model has no default any more (the caller must state the provider-
// qualified model explicitly; fundi's core invents nothing).
func TestParseAgentFlagsRequiresModel(t *testing.T) {
	if _, err := parseAgentFlags(nil); err == nil {
		t.Fatal("parseAgentFlags(nil) with no --model: want error, got nil")
	}
}

// TestParseAgentFlagsRequiresSlash covers the other half of the invariant: a
// bare (non-provider-qualified) --model is also rejected, since fundi does
// not rely on rafiki's bare-id backward-compat resolution.
func TestParseAgentFlagsRequiresSlash(t *testing.T) {
	if _, err := parseAgentFlags([]string{"--model", "sonnet-latest"}); err == nil {
		t.Fatal("parseAgentFlags with a bare (non-provider-qualified) --model: want error, got nil")
	}
}

func TestParseAgentFlagsModelAndThinkingDefault(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.model != "anthropic/sonnet-latest" {
		t.Errorf("model = %q, want anthropic/sonnet-latest", f.model)
	}
	if f.thinking != "off" {
		t.Errorf("thinking = %q, want off", f.thinking)
	}
}

func TestParseAgentFlagsRefFromEnv(t *testing.T) {
	t.Setenv("PI_CONTROLLER_CHILD_ID", "child-123")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "child-123" {
		t.Errorf("ref = %q, want child-123 (from $PI_CONTROLLER_CHILD_ID)", f.ref)
	}
}

func TestParseAgentFlagsRefExplicitFlagWins(t *testing.T) {
	t.Setenv("PI_CONTROLLER_CHILD_ID", "child-123")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--ref", "explicit-ref"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "explicit-ref" {
		t.Errorf("ref = %q, want explicit-ref (explicit --ref overrides the env default)", f.ref)
	}
}

func TestParseAgentFlagsDBFromEnv(t *testing.T) {
	t.Setenv("FUNDI_AGENT_DB", "postgres://example/db")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.db != "postgres://example/db" {
		t.Errorf("db = %q, want postgres://example/db (from $FUNDI_AGENT_DB)", f.db)
	}
}

func TestParseAgentFlagsRepeatableSkillsDir(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--skills-dir", "/a", "--skills-dir", "/b"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if len(f.skillsDir) != 2 || f.skillsDir[0] != "/a" || f.skillsDir[1] != "/b" {
		t.Errorf("skillsDir = %v, want [/a /b]", f.skillsDir)
	}
}

func TestParseAgentFlagsThinkingLevel(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--thinking", "xhigh"})
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
	if _, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--not-a-real-flag"}); err == nil {
		t.Fatal("parseAgentFlags with an unknown flag: want error, got nil")
	}
}

func TestParseAgentFlagsNoSkillsAndNoContextFiles(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--no-skills", "--no-context-files"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if !f.noSkills || !f.noContextFiles {
		t.Errorf("noSkills=%v noContextFiles=%v, want both true", f.noSkills, f.noContextFiles)
	}
}
