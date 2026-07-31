package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/agent"
	skillspkg "go.graveland.dev/rafiki/pkg/skills"
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

// TestAssembleSkillDirs_NoClaudeHomeDir locks down the config-ownership
// invariant this task exists for: fundi must never read skills out of the
// user's home Claude profile (~/.claude/skills). It deliberately does NOT
// forbid a per-project .claude/skills dir - a repo that already has one
// keeps working, per the ruling in task-A4-brief.md's override. The project
// .fundi/skills dir comes after .claude/skills so it wins on name collision.
func TestAssembleSkillDirs_NoClaudeHomeDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("FUNDI_SKILLS_DIRS", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")

	dirs := assembleSkillDirs("/work/repo", nil)

	for _, d := range dirs {
		if strings.HasPrefix(d, "/home/testuser") {
			t.Errorf("skill dir must not be under the user's home Claude profile: %s", d)
		}
	}
	if len(dirs) != 3 {
		t.Fatalf("dirs = %v, want 3 entries", dirs)
	}
	if dirs[0] != "/tmp/cfg/fundi/skills" {
		t.Errorf("dirs[0] = %q, want /tmp/cfg/fundi/skills", dirs[0])
	}
	if dirs[1] != "/work/repo/.claude/skills" {
		t.Errorf("dirs[1] = %q, want /work/repo/.claude/skills (existing per-project skills keep working)", dirs[1])
	}
	if dirs[2] != "/work/repo/.fundi/skills" {
		t.Errorf("dirs[2] = %q, want /work/repo/.fundi/skills (fundi's own per-project dir, overrides .claude)", dirs[2])
	}
}

func TestAssembleSkillDirs_FlagsWinLast(t *testing.T) {
	t.Setenv("FUNDI_SKILLS_DIRS", "/env/skills")
	dirs := assembleSkillDirs("/work/repo", []string{"/flag/skills"})
	if dirs[len(dirs)-1] != "/flag/skills" {
		t.Errorf("--skills-dir must have highest precedence, got %v", dirs)
	}
}

// TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision proves the whole
// point of reading both per-project dirs: when a skill of the same name
// exists under both .claude/skills and .fundi/skills, the .fundi one wins.
// This exercises the real merge in skillspkg.DiscoverSkills (later dir wins),
// not just the ordering of assembleSkillDirs's output slice.
func TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision(t *testing.T) {
	repo := t.TempDir()
	claudeSkillDir := filepath.Join(repo, ".claude", "skills", "demo")
	fundiSkillDir := filepath.Join(repo, ".fundi", "skills", "demo")
	if err := os.MkdirAll(claudeSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .claude skill: %v", err)
	}
	if err := os.MkdirAll(fundiSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .fundi skill: %v", err)
	}
	claudeFrontmatter := "---\nname: demo\ndescription: from .claude\n---\n"
	fundiFrontmatter := "---\nname: demo\ndescription: from .fundi\n---\n"
	if err := os.WriteFile(filepath.Join(claudeSkillDir, "SKILL.md"), []byte(claudeFrontmatter), 0o644); err != nil {
		t.Fatalf("WriteFile .claude SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fundiSkillDir, "SKILL.md"), []byte(fundiFrontmatter), 0o644); err != nil {
		t.Fatalf("WriteFile .fundi SKILL.md: %v", err)
	}

	t.Setenv("FUNDI_SKILLS_DIRS", "") // isolate from the invoking user's real config dir

	dirs := assembleSkillDirs(repo, nil)
	skills, err := skillspkg.DiscoverSkills(dirs, nil)
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}

	var demo *skillspkg.SkillMeta
	for i := range skills {
		if skills[i].Name == "demo" {
			demo = &skills[i]
		}
	}
	if demo == nil {
		t.Fatalf("skill %q not found in %v", "demo", skills)
	}
	if demo.Description != "from .fundi" {
		t.Errorf("demo.Description = %q, want %q (.fundi/skills must win over .claude/skills on name collision)", demo.Description, "from .fundi")
	}
}

// countingCloser records Close calls and can be told to fail with a specific
// error, so the standaloneFatal test can cover both the ordinary close and the
// already-closed case it must tolerate.
type countingCloser struct {
	mu     sync.Mutex
	closes int
	err    error
}

func (c *countingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return c.err
}

func (c *countingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// TestStandaloneFatalEndsTheProcess covers the OnFatal hook `fundid agent`
// passes to agent.RuntimeOptions. Before it existed the standalone path built
// its RuntimeOptions with no OnFatal at all, so a turn panic marked the engine
// dead, logged, and left the process answering get_state forever while every
// prompt was silently dropped — the exact silently-stopped-queue shape the
// daemon path was fixed to avoid. A nil OnFatal is documented as legal, but here
// it was a choice rather than a constraint.
//
// The hook's whole job is to unblock Frontend.Run, which is parked reading
// stdin; closing stdin is the only way to do that.
func TestStandaloneFatalEndsTheProcess(t *testing.T) {
	silenceStandaloneLogs(t)
	stdin := &countingCloser{}
	hook, fired := standaloneFatal(stdin)

	want := errors.New("turn panicked")
	hook(want)

	select {
	case got := <-fired:
		if !errors.Is(got, want) {
			t.Errorf("fired error = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standaloneFatal never reported the fatal error; runAgent would exit 0 as if nothing happened")
	}
	if got := stdin.count(); got != 1 {
		t.Errorf("stdin closed %d times, want 1; without it Frontend.Run stays parked on a read and the process never ends", got)
	}

	// OnFatal is once-only by contract, but the send is into a size-1 buffer and
	// the close is on a real file: a second call must be a no-op either way.
	hook(errors.New("second"))
	if got := stdin.count(); got != 1 {
		t.Errorf("stdin closed %d times after a second hook call, want 1", got)
	}
	select {
	case got := <-fired:
		t.Errorf("a second hook call reported %v; the hook must fire once", got)
	default:
	}
}

// TestStandaloneFatalToleratesAnAlreadyClosedStdin: stdin may already be closed
// (a racing EOF), and that must not be logged as a failure or stop the hook from
// reporting the fatal error.
func TestStandaloneFatalToleratesAnAlreadyClosedStdin(t *testing.T) {
	silenceStandaloneLogs(t)
	stdin := &countingCloser{err: os.ErrClosed}
	hook, fired := standaloneFatal(stdin)

	hook(errors.New("boom"))
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("standaloneFatal did not report the fatal error when stdin was already closed")
	}
}

// silenceStandaloneLogs keeps the error-level logging standaloneFatal does out
// of the test output.
func silenceStandaloneLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
