package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi"
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
	t.Setenv("RAFIKI_CHILD_ID", "child-123")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "child-123" {
		t.Errorf("ref = %q, want child-123 (from $RAFIKI_CHILD_ID)", f.ref)
	}
}

func TestParseAgentFlagsRefExplicitFlagWins(t *testing.T) {
	t.Setenv("RAFIKI_CHILD_ID", "child-123")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--ref", "explicit-ref"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.ref != "explicit-ref" {
		t.Errorf("ref = %q, want explicit-ref (explicit --ref overrides the env default)", f.ref)
	}
}

func TestParseAgentFlagsDBFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://example/db")
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.db != "postgres://example/db" {
		t.Errorf("db = %q, want postgres://example/db (from $RAFIKI_DB)", f.db)
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
	if _, err := fundi.ThinkingBudgetFor(f.thinking); err != nil {
		t.Errorf("ThinkingBudgetFor(%q): unexpected error: %v", f.thinking, err)
	}
}

// TestParseAgentFlagsRecordRequests guards the `rafikid fundi --record-requests`
// flag: parseAgentFlags must actually set f.recordRequests, since runAgent
// gates whether opts.RawTrace is populated on this field (and previously did
// not read it at all — the flag parsed but silently did nothing).
func TestParseAgentFlagsRecordRequests(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--record-requests"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if !f.recordRequests {
		t.Error("recordRequests = false, want true")
	}

	f2, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f2.recordRequests {
		t.Error("recordRequests = true, want false (flag not passed)")
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
// invariant this task exists for: rafiki must never read skills out of the
// user's home Claude profile (~/.claude/skills). It deliberately does NOT
// forbid a per-project .claude/skills dir - a repo that already has one
// keeps working, per the ruling in task-A4-brief.md's override. The project
// .rafiki/skills dir comes after .claude/skills so it wins on name collision.
func TestAssembleSkillDirs_NoClaudeHomeDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("RAFIKI_SKILLS_DIRS", "")
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
	if dirs[0] != "/tmp/cfg/rafiki/skills" {
		t.Errorf("dirs[0] = %q, want /tmp/cfg/rafiki/skills", dirs[0])
	}
	if dirs[1] != "/work/repo/.claude/skills" {
		t.Errorf("dirs[1] = %q, want /work/repo/.claude/skills (existing per-project skills keep working)", dirs[1])
	}
	if dirs[2] != "/work/repo/.rafiki/skills" {
		t.Errorf("dirs[2] = %q, want /work/repo/.rafiki/skills (rafiki's own per-project dir, overrides .claude)", dirs[2])
	}
}

func TestAssembleSkillDirs_FlagsWinLast(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "/env/skills")
	dirs := assembleSkillDirs("/work/repo", []string{"/flag/skills"})
	if dirs[len(dirs)-1] != "/flag/skills" {
		t.Errorf("--skills-dir must have highest precedence, got %v", dirs)
	}
}

// TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision proves the whole
// point of reading both per-project dirs: when a skill of the same name
// exists under both .claude/skills and .rafiki/skills, the .rafiki one wins.
// This exercises the real merge in skillspkg.DiscoverSkills (later dir wins),
// not just the ordering of assembleSkillDirs's output slice.
func TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision(t *testing.T) {
	repo := t.TempDir()
	claudeSkillDir := filepath.Join(repo, ".claude", "skills", "demo")
	rafikiSkillDir := filepath.Join(repo, ".rafiki", "skills", "demo")
	if err := os.MkdirAll(claudeSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .claude skill: %v", err)
	}
	if err := os.MkdirAll(rafikiSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .rafiki skill: %v", err)
	}
	claudeFrontmatter := "---\nname: demo\ndescription: from .claude\n---\n"
	rafikiFrontmatter := "---\nname: demo\ndescription: from .rafiki\n---\n"
	if err := os.WriteFile(filepath.Join(claudeSkillDir, "SKILL.md"), []byte(claudeFrontmatter), 0o644); err != nil {
		t.Fatalf("WriteFile .claude SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rafikiSkillDir, "SKILL.md"), []byte(rafikiFrontmatter), 0o644); err != nil {
		t.Fatalf("WriteFile .rafiki SKILL.md: %v", err)
	}

	t.Setenv("RAFIKI_SKILLS_DIRS", "") // isolate from the invoking user's real config dir

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
	if demo.Description != "from .rafiki" {
		t.Errorf("demo.Description = %q, want %q (.rafiki/skills must win over .claude/skills on name collision)", demo.Description, "from .rafiki")
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

// TestStandaloneFatalEndsTheProcess covers the OnFatal hook `rafikid agent`
// passes to fundi.RuntimeOptions. Before it existed the standalone path built
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

// TestBashRTKValuePrecedence verifies the --bash-rtk flag precedence:
// explicit flag beats $RAFIKI_BASH_RTK beats the "auto" default.
func TestBashRTKValuePrecedence(t *testing.T) {
	// Default (no flag, no env) → "auto"
	t.Run("default", func(t *testing.T) {
		t.Setenv("RAFIKI_BASH_RTK", "")
		if got := bashRTKValue(""); got != "auto" {
			t.Errorf("bashRTKValue(\"\") = %q, want auto", got)
		}
	})

	// Env var only → use env var
	t.Run("env", func(t *testing.T) {
		t.Setenv("RAFIKI_BASH_RTK", "off")
		if got := bashRTKValue(""); got != "off" {
			t.Errorf("bashRTKValue(\"\") = %q, want off", got)
		}
	})

	// Explicit flag beats env var
	t.Run("flag-beats-env", func(t *testing.T) {
		t.Setenv("RAFIKI_BASH_RTK", "off")
		if got := bashRTKValue("on"); got != "on" {
			t.Errorf("bashRTKValue(\"on\") = %q, want on (explicit flag must beat env)", got)
		}
	})
}

// TestParseAgentFlagsBashRTK verifies the --bash-rtk flag is actually read
// by parseAgentFlags. This is the exact check that would have caught
// --record-requests before it shipped parsed-but-unread.
func TestParseAgentFlagsBashRTK(t *testing.T) {
	// Flag passed → f.bashRTK is set
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--bash-rtk", "on"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.bashRTK != "on" {
		t.Errorf("bashRTK = %q, want on", f.bashRTK)
	}

	// Flag not passed → empty
	f2, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f2.bashRTK != "" {
		t.Errorf("bashRTK = %q, want empty (flag not passed)", f2.bashRTK)
	}
}

// TestToolsWebValuePrecedence verifies the --tools-web flag precedence added
// for finding 17: explicit flag beats $RAFIKI_TOOLS_WEB beats the "off"
// default. Unlike --bash-rtk's three real states (auto/on/off), the whole
// point of this flag is being able to force the toggle OFF even when the env
// var says on — a plain flag.BoolVar cannot express "not passed" separately
// from "passed false", so both directions are asserted here, not just
// flag-wins-when-on.
func TestToolsWebValuePrecedence(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("RAFIKI_TOOLS_WEB", "")
		if got := toolsWebValue(""); got != false {
			t.Errorf("toolsWebValue(\"\") = %v, want false", got)
		}
	})

	t.Run("env-only", func(t *testing.T) {
		t.Setenv("RAFIKI_TOOLS_WEB", "1")
		if got := toolsWebValue(""); got != true {
			t.Errorf("toolsWebValue(\"\") = %v, want true (from $RAFIKI_TOOLS_WEB=1)", got)
		}
	})

	t.Run("flag-on-beats-env-unset", func(t *testing.T) {
		t.Setenv("RAFIKI_TOOLS_WEB", "")
		if got := toolsWebValue("on"); got != true {
			t.Errorf("toolsWebValue(\"on\") = %v, want true", got)
		}
	})

	// The direction a bool flag cannot express: turning the toggle OFF via
	// the flag while the env var says on.
	t.Run("flag-off-beats-env-on", func(t *testing.T) {
		t.Setenv("RAFIKI_TOOLS_WEB", "1")
		if got := toolsWebValue("off"); got != false {
			t.Errorf("toolsWebValue(\"off\") = %v, want false (explicit flag must beat $RAFIKI_TOOLS_WEB=1)", got)
		}
	})
}

// TestParseAgentFlagsToolsWeb verifies --tools-web is actually read by
// parseAgentFlags, the same regression class TestParseAgentFlagsBashRTK
// guards against (a flag registered but never read from the parsed struct).
func TestParseAgentFlagsToolsWeb(t *testing.T) {
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--tools-web", "on"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f.toolsWeb != "on" {
		t.Errorf("toolsWeb = %q, want on", f.toolsWeb)
	}

	f2, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	if f2.toolsWeb != "" {
		t.Errorf("toolsWeb = %q, want empty (flag not passed)", f2.toolsWeb)
	}
}

// TestEffectiveLSPConfigPrecedence covers finding 15's extracted helper: an
// explicit --lsp-config must survive a missing file (so BuildRuntime raises
// the hard error), while a defaulted path that doesn't exist must be blanked
// (so a cwd with no lsp.json just runs without LSP tools, as before).
func TestEffectiveLSPConfigPrecedence(t *testing.T) {
	t.Run("explicit missing path is preserved", func(t *testing.T) {
		got := effectiveLSPConfig("/does/not/exist/lsp.json", t.TempDir())
		if got != "/does/not/exist/lsp.json" {
			t.Errorf("effectiveLSPConfig = %q, want the explicit path preserved so BuildRuntime raises it", got)
		}
	})

	t.Run("defaulted missing path is blanked", func(t *testing.T) {
		cwd := t.TempDir() // no .lsp.json written
		if got := effectiveLSPConfig("", cwd); got != "" {
			t.Errorf("effectiveLSPConfig(\"\", %q) = %q, want empty (defaulted path absent: skip LSP)", cwd, got)
		}
	})

	t.Run("defaulted present path is kept", func(t *testing.T) {
		cwd := t.TempDir()
		cwdCfg := filepath.Join(cwd, ".lsp.json")
		if err := os.WriteFile(cwdCfg, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := effectiveLSPConfig("", cwd); got != cwdCfg {
			t.Errorf("effectiveLSPConfig(\"\", %q) = %q, want %q", cwd, got, cwdCfg)
		}
	})

	t.Run("explicit existing path is kept", func(t *testing.T) {
		cwd := t.TempDir()
		explicit := filepath.Join(t.TempDir(), "custom-lsp.json")
		if err := os.WriteFile(explicit, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := effectiveLSPConfig(explicit, cwd); got != explicit {
			t.Errorf("effectiveLSPConfig(%q, %q) = %q, want %q", explicit, cwd, got, explicit)
		}
	})
}

// TestLSPAndToolsWebHelpersSharedAcrossCallSites is the regression guard for
// finding 15 itself: it proves runAgent (agent.go) and toRuntimeOptions
// (agent_runtime.go) resolve LSPConfig/ToolsWeb through the same two helpers,
// rather than through hand-copied implementations that merely happen to agree
// today. A behavioural test cannot cover this on its own — toRuntimeOptions is
// callable from a test but runAgent is not (it is a signal- and stdin-driven
// CLI entrypoint returning an exit code), so the daemon half can be asserted
// on outputs and the standalone half can only be asserted structurally.
//
// This walks the AST rather than grepping the source text: a textual check for
// the call expression breaks on a local variable rename, on gofmt wrapping the
// call across lines, or on an added argument, none of which reintroduce the
// duplication this is meant to catch.
func TestLSPAndToolsWebHelpersSharedAcrossCallSites(t *testing.T) {
	wantCalls := []string{"effectiveLSPConfig", "toolsWebValue"}

	for file, fn := range map[string]string{
		"agent.go":         "runAgent",
		"agent_runtime.go": "toRuntimeOptions",
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		var body *ast.FuncDecl
		for _, decl := range parsed.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				body = fd
				break
			}
		}
		if body == nil {
			t.Fatalf("%s: no func %s; this test needs updating alongside the rename", file, fn)
		}

		called := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok {
					called[ident.Name] = true
				}
			}
			return true
		})

		for _, want := range wantCalls {
			if !called[want] {
				t.Errorf("%s: %s does not call %s; finding 15's duplicated resolution logic is back",
					file, fn, want)
			}
		}
	}
}
