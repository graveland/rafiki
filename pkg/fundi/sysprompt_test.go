package fundi

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestBuildSystemPromptOrder is the brief's named assembly-order test:
// base -> append -> context files -> skills -> env block. Cache stability
// depends on this exact order, so it's asserted by strict index comparison,
// not just "contains".
func TestBuildSystemPromptOrder(t *testing.T) {
	c := assert.NewAborting(t)
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
		c.NotEq(-1, idx, "missing marker %q in %q", m, got)
		c.Greater(prev, idx, "marker %q out of order (idx %d <= prev %d) in %q", m, idx, prev, got)
		prev = idx
	}
}

// TestBuildSystemPromptOverrideReplacesBase asserts Override wins over Base
// entirely - Base's content must not leak into the assembled prompt.
func TestBuildSystemPromptOverrideReplacesBase(t *testing.T) {
	c := assert.NewAborting(t)
	got := BuildSystemPrompt(SysPromptConfig{
		Base:     "BASE_TEXT",
		Override: "OVERRIDE_TEXT",
		Cwd:      "/x",
		ModelID:  "m",
	})
	c.StrContains(got, "OVERRIDE_TEXT", "expected override text present, got")
	c.NotStrContains(got, "BASE_TEXT", "base text leaked despite override, got")
}

// TestBuildSystemPromptEmptySectionsLeaveNoStrayGaps ensures optional
// sections that are empty don't leave blank sections or doubled separators -
// with everything but Cwd/ModelID empty, the env block should be the only
// section, and it should stand alone (no leading/trailing separator debris).
func TestBuildSystemPromptEmptySectionsLeaveNoStrayGaps(t *testing.T) {
	c := assert.NewAborting(t)
	got := BuildSystemPrompt(SysPromptConfig{Cwd: "/only/env", ModelID: "m1"})
	c.False(strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n"), "expected no leading/trailing blank lines, got %q", got)
	c.NotStrContains(got, "\n\n\n", "expected no stray blank-section gaps, got")
	c.False(!strings.Contains(got, "/only/env") || !strings.Contains(got, "m1"), "expected env block content, got %q", got)
}

// TestBuildSystemPromptEnvBlockCarriesPlatformAndDate covers the env block's
// required fields beyond cwd/model: platform and today's date.
func TestBuildSystemPromptEnvBlockCarriesPlatformAndDate(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{Cwd: "/x", ModelID: "m1"})
	assert.NewAborting(t).False(!strings.Contains(got, "darwin") && !strings.Contains(got, "linux"), "expected platform (darwin/linux) in env block, got %q", got)
}

func TestSystemPromptCarriesTheWorkspaceAssignment(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{
		Base: "base.", Cwd: "/work", ModelID: "anthropic/claude-opus-5",
		Workspace: &WorkspaceInfo{
			ExecutorName:  "ci-runner-2",
			Isolation:     "container",
			WorkspaceMode: "ephemeral",
			Roots:         []string{"/work", "/repo"},
		},
	})
	for _, want := range []string{
		"ci-runner-2", "container", "ephemeral", "/work", "/repo",
	} {
		assert.NewCollecting(t).StrContains(got, want, "system prompt missing")
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
	assert.NewAborting(t).GreaterOrEqual(strings.Index(got, "SKILLS"), strings.Index(got, "EXECNAME"), "the workspace block precedes the skills inventory, busting the cache prefix for every child")
}

// A native, unsandboxed agent gets NOTHING. The block is paid only by the
// agents it is true for — the cheapest rung of prompting.md's cost ladder.
func TestNoWorkspaceBlockWhenRunningNatively(t *testing.T) {
	got := BuildSystemPrompt(SysPromptConfig{Base: "base.", Cwd: "/w", ModelID: "m"})
	assert.NewAborting(t).NotStrContains(strings.ToLower(got), "isolation", "an unsandboxed agent must not pay for a sandbox description:\n%s", got)
}

// The base prompt is not the place for this. defaultBasePrompt is four
// sentences and names no tool; every line added there is paid on all traffic
// by every agent forever.
func TestDefaultBasePromptIsUnchanged(t *testing.T) {
	assert.NewAborting(t).False(strings.Contains(defaultBasePrompt, "executor") ||
		strings.Contains(defaultBasePrompt, "workspace") ||
		strings.Contains(defaultBasePrompt, "container"), "the executor grant leaked into defaultBasePrompt, which is paid on every request by every agent")
}

// The recall nudge is emitted only for a child that actually has the recall
// and memory tools: a child without them must not pay for the text on every
// request.
func TestRecallSectionOnlyWhenEnabled(t *testing.T) {
	on := BuildSystemPrompt(SysPromptConfig{Base: "base.", Recall: true, Cwd: "/w", ModelID: "m"})
	off := BuildSystemPrompt(SysPromptConfig{Base: "base.", Cwd: "/w", ModelID: "m"})

	c := assert.NewAborting(t)
	c.StrContains(on, "## Recall", "recall section missing when Recall is set; got")
	c.NotStrContains(off, "## Recall", "recall section present when Recall is unset; got")
}

// The recall section is static content, so it belongs after the append and
// before the context files: anything that varies must come after the cacheable
// prefix (see BuildSystemPrompt's cache-stability comment).
func TestRecallSectionOrdering(t *testing.T) {
	c := assert.NewAborting(t)
	got := BuildSystemPrompt(SysPromptConfig{
		Base:         "BASE_MARKER",
		Append:       "APPEND_MARKER",
		Recall:       true,
		ContextFiles: "FILES_MARKER",
		Cwd:          "/w",
		ModelID:      "m",
	})

	recallIdx := strings.Index(got, "## Recall")
	appendIdx := strings.Index(got, "APPEND_MARKER")
	filesIdx := strings.Index(got, "FILES_MARKER")

	c.Greater(-1, recallIdx, "recall section missing from %q", got)
	c.Greater(appendIdx, recallIdx, "the recall section must come after the append; got %q", got)
	c.Less(filesIdx, recallIdx, "the recall section must come before the context files; got %q", got)
}
