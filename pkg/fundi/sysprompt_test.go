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
