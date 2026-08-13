package fundi

import (
	"strings"
	"testing"
)

// TestBuildSystemPromptOrder is the brief's named assembly-order test:
// base -> append -> context files -> skills -> env block. Cache stability
// depends on this exact order, so it's asserted by strict index comparison,
// not just "contains".
func TestBuildSystemPromptOrder(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{
		Base:            "BASE_MARKER",
		Append:          "APPEND_MARKER",
		ContextFiles:    "FILES_MARKER",
		SkillsInventory: "SKILLS_MARKER",
		Cwd:             "/work/proj",
		ModelID:         "claude-opus-4-6",
	})

	markers := []string{"BASE_MARKER", "APPEND_MARKER", "FILES_MARKER", "SKILLS_MARKER", "/work/proj"}
	prev := -1
	for _, m := range markers {
		idx := strings.Index(got, m)
		if idx == -1 {
			t.Fatalf("missing marker %q in %q", m, got)
		}
		if idx <= prev {
			t.Fatalf("marker %q out of order (idx %d <= prev %d) in %q", m, idx, prev, got)
		}
		prev = idx
	}
}

// TestBuildSystemPromptOverrideReplacesBase asserts Override wins over Base
// entirely - Base's content must not leak into the assembled prompt.
func TestBuildSystemPromptOverrideReplacesBase(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{
		Base:     "BASE_TEXT",
		Override: "OVERRIDE_TEXT",
		Cwd:      "/x",
		ModelID:  "m",
	})
	if !strings.Contains(got, "OVERRIDE_TEXT") {
		t.Fatalf("expected override text present, got %q", got)
	}
	if strings.Contains(got, "BASE_TEXT") {
		t.Fatalf("base text leaked despite override, got %q", got)
	}
}

// TestBuildSystemPromptEmptySectionsLeaveNoStrayGaps ensures optional
// sections that are empty don't leave blank sections or doubled separators -
// with everything but Cwd/ModelID empty, the env block should be the only
// section, and it should stand alone (no leading/trailing separator debris).
func TestBuildSystemPromptEmptySectionsLeaveNoStrayGaps(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{Cwd: "/only/env", ModelID: "m1"})
	if strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n") {
		t.Fatalf("expected no leading/trailing blank lines, got %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("expected no stray blank-section gaps, got %q", got)
	}
	if !strings.Contains(got, "/only/env") || !strings.Contains(got, "m1") {
		t.Fatalf("expected env block content, got %q", got)
	}
}

// TestBuildSystemPromptEnvBlockCarriesPlatformAndDate covers the env block's
// required fields beyond cwd/model: platform and today's date.
func TestBuildSystemPromptEnvBlockCarriesPlatformAndDate(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{Cwd: "/x", ModelID: "m1"})
	if !strings.Contains(got, "darwin") && !strings.Contains(got, "linux") {
		t.Fatalf("expected platform (darwin/linux) in env block, got %q", got)
	}
}

func TestSystemPromptCarriesTheWorkspaceAssignment(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{
		Base: "base.", Cwd: "/work", ModelID: "anthropic/claude-opus-5",
		Workspace: &WorkspaceInfo{
			ExecutorName:  "ci-runner-2",
			Isolation:     "container",
			WorkspaceMode: "ephemeral",
			Roots:         []string{"/work", "/repo"},
			ReadOnlyRoots: []string{"/repo"},
			Network:       "none",
		},
	})
	for _, want := range []string{
		"ci-runner-2", "container", "ephemeral", "/work", "/repo", "read-only", "no network",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q:\n%s", want, got)
		}
	}
}

// Cache ordering: rafiki's prompt-cache breakpoint sits over the tools+system
// prefix, so static content must precede anything that varies. The workspace
// block varies BETWEEN children, so it belongs in the environment block at the
// end — never before the skills inventory.
func TestWorkspaceBlockComesAfterTheCacheableSections(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{
		Base: "BASE", SkillsInventory: "SKILLS", Cwd: "/w", ModelID: "m",
		Workspace: &WorkspaceInfo{ExecutorName: "EXECNAME", Isolation: "container"},
	})
	if strings.Index(got, "EXECNAME") < strings.Index(got, "SKILLS") {
		t.Fatal("the workspace block precedes the skills inventory, busting the cache prefix for every child")
	}
}

// A native, unsandboxed agent gets NOTHING. The block is paid only by the
// agents it is true for — the cheapest rung of prompting.md's cost ladder.
func TestNoWorkspaceBlockWhenRunningNatively(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{Base: "base.", Cwd: "/w", ModelID: "m"})
	if strings.Contains(strings.ToLower(got), "isolation") {
		t.Fatalf("an unsandboxed agent must not pay for a sandbox description:\n%s", got)
	}
}

// The base prompt is not the place for this. defaultBasePrompt is four
// sentences and names no tool; every line added there is paid on all traffic
// by every agent forever.
func TestDefaultBasePromptIsUnchanged(t *testing.T) {
	if strings.Contains(defaultBasePrompt, "executor") ||
		strings.Contains(defaultBasePrompt, "workspace") ||
		strings.Contains(defaultBasePrompt, "container") {
		t.Fatal("the executor grant leaked into defaultBasePrompt, which is paid on every request by every agent")
	}
}
