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

	"github.com/multigres/testkit/assert"
)

// TestParseAgentFlagsRequiresModel covers the redesign's central invariant:
// --model has no default any more (the caller must state the provider-
// qualified model explicitly; fundi's core invents nothing).
func TestParseAgentFlagsRequiresModel(t *testing.T) {
	_, err := parseAgentFlags(nil)
	assert.NewAborting(t).Error(err, "parseAgentFlags(nil) with no --model: want error, got nil")
}

// TestParseAgentFlagsRequiresSlash covers the other half of the invariant: a
// bare (non-provider-qualified) --model is also rejected, since fundi does
// not rely on rafiki's bare-id backward-compat resolution.
func TestParseAgentFlagsRequiresSlash(t *testing.T) {
	_, err := parseAgentFlags([]string{"--model", "sonnet-latest"})
	assert.NewAborting(t).Error(err, "parseAgentFlags with a bare (non-provider-qualified) --model: want error, got nil")
}

func TestParseAgentFlagsModelAndThinkingDefault(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("anthropic/sonnet-latest", f.model, "model")
	c.Eq("off", f.thinking, "thinking")
}

func TestParseAgentFlagsRefFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_CHILD_ID", "child-123")
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("child-123", f.ref, "ref")
}

func TestParseAgentFlagsRefExplicitFlagWins(t *testing.T) {
	t.Setenv("RAFIKI_CHILD_ID", "child-123")
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--ref", "explicit-ref"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("explicit-ref", f.ref, "ref")
}

func TestParseAgentFlagsDBFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://example/db")
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("postgres://example/db", f.db, "db")
}

func TestParseAgentFlagsRepeatableSkillsDir(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--skills-dir", "/a", "--skills-dir", "/b"})
	c.Require().NoError(err, "parseAgentFlags")
	c.False(len(f.skillsDir) != 2 || f.skillsDir[0] != "/a" || f.skillsDir[1] != "/b", "skillsDir = %v, want [/a /b]", f.skillsDir)
}

func TestParseAgentFlagsThinkingLevel(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--thinking", "xhigh"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("xhigh", f.thinking, "thinking")
	if _, err := fundi.ThinkingBudgetFor(f.thinking); err != nil {
		t.Errorf("ThinkingBudgetFor(%q): unexpected error: %v", f.thinking, err)
	}
}

// TestParseAgentFlagsRecordRequests guards the `rafikid fundi --record-requests`
// flag: parseAgentFlags must actually set f.recordRequests, since runAgent
// gates whether opts.RawTrace is populated on this field (and previously did
// not read it at all — the flag parsed but silently did nothing).
func TestParseAgentFlagsRecordRequests(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--record-requests"})
	c.Require().NoError(err, "parseAgentFlags")
	c.True(f.recordRequests, "recordRequests = false, want true")

	f2, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.False(f2.recordRequests, "recordRequests = true, want false (flag not passed)")
}

func TestParseAgentFlagsRejectsUnknownFlag(t *testing.T) {
	_, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--not-a-real-flag"})
	assert.NewAborting(t).Error(err, "parseAgentFlags with an unknown flag: want error, got nil")
}

func TestParseAgentFlagsNoSkillsAndNoContextFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--no-skills", "--no-context-files"})
	c.Require().NoError(err, "parseAgentFlags")
	c.False(!f.noSkills || !f.noContextFiles, "noSkills=%v noContextFiles=%v, want both true", f.noSkills, f.noContextFiles)
}

func TestParseAgentFlags_MCPServersAndNoMCP(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--model", "anthropic/claude-sonnet-5", "--mcp-servers", "codescan,other", "--no-mcp"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("codescan,other", f.mcpServers, "mcpServers")
	c.True(f.noMCP, "noMCP = false, want true")
}

// TestAssembleSkillDirs_NoClaudeHomeDir locks down the config-ownership
// invariant this task exists for: rafiki must never read skills out of the
// user's home Claude profile (~/.claude/skills). It deliberately does NOT
// forbid a per-project .claude/skills dir - a repo that already has one
// keeps working, per the ruling in task-A4-brief.md's override. The project
// .rafiki/skills dir comes after .claude/skills so it wins on name collision.
//
// Since the database became the curated tier, the daemon-local
// <ConfigDir>/skills default is OPT-IN (assembleSkillDirs): an unset
// RAFIKI_SKILLS_DIRS yields only the per-project directories, so this test
// also pins that the default config dir is no longer added unconditionally.
func TestAssembleSkillDirs_NoClaudeHomeDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("RAFIKI_SKILLS_DIRS", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	c := assert.NewCollecting(t)

	dirs := assembleSkillDirs("/work/repo", nil, false)

	for _, d := range dirs {
		c.False(strings.HasPrefix(d, "/home/testuser"), "skill dir must not be under the user's home Claude profile: %s", d)
	}
	c.Require().Len(dirs, 2, "dirs")
	c.Eq("/work/repo/.claude/skills", dirs[0], "dirs[0]")
	c.Eq("/work/repo/.rafiki/skills", dirs[1], "dirs[1]")
}

func TestAssembleSkillDirs_FlagsWinLast(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "/env/skills")
	dirs := assembleSkillDirs("/work/repo", []string{"/flag/skills"}, false)
	assert.NewCollecting(t).Eq("/flag/skills", dirs[len(dirs)-1], "--skills-dir must have highest precedence, got %v", dirs)
}

// TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision proves the whole
// point of reading both per-project dirs: when a skill of the same name
// exists under both .claude/skills and .rafiki/skills, the .rafiki one wins.
// This exercises the real merge in skillspkg.DiscoverSkills (later dir wins),
// not just the ordering of assembleSkillDirs's output slice.
func TestAssembleSkillDirs_FundiBeatsClaudeOnNameCollision(t *testing.T) {
	c := assert.NewCollecting(t)
	repo := t.TempDir()
	claudeSkillDir := filepath.Join(repo, ".claude", "skills", "demo")
	rafikiSkillDir := filepath.Join(repo, ".rafiki", "skills", "demo")
	c.Require().NoError(os.MkdirAll(claudeSkillDir, 0o755), "MkdirAll .claude skill")
	c.Require().NoError(os.MkdirAll(rafikiSkillDir, 0o755), "MkdirAll .rafiki skill")
	claudeFrontmatter := "---\nname: demo\ndescription: from .claude\n---\n"
	rafikiFrontmatter := "---\nname: demo\ndescription: from .rafiki\n---\n"
	c.Require().NoError(os.WriteFile(filepath.Join(claudeSkillDir, "SKILL.md"), []byte(claudeFrontmatter), 0o644), "WriteFile .claude SKILL.md")
	c.Require().NoError(os.WriteFile(filepath.Join(rafikiSkillDir, "SKILL.md"), []byte(rafikiFrontmatter), 0o644), "WriteFile .rafiki SKILL.md")

	t.Setenv("RAFIKI_SKILLS_DIRS", "") // isolate from the invoking user's real config dir

	dirs := assembleSkillDirs(repo, nil, false)
	skills, err := skillspkg.DiscoverSkills(dirs, nil)
	c.Require().NoError(err, "DiscoverSkills")

	var demo *skillspkg.SkillMeta
	for i := range skills {
		if skills[i].Name == "demo" {
			demo = &skills[i]
		}
	}
	c.Require().NotNil(demo, "skill %q not found in %v", "demo", skills)
	c.Eq("from .rafiki", demo.Description, "demo.Description")
}

// When hasExecutor is true, the project-tier directories are omitted because
// cwd names a path on the executor's machine. Finding them on the daemon
// would return either nothing or a different project's skills.
func TestAssembleSkillDirs_DropsProjectTierWithExecutor(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "")

	dirs := assembleSkillDirs("/work/repo", nil, true)

	for _, d := range dirs {
		assert.NewCollecting(t).False(strings.Contains(d, ".claude/skills") || strings.Contains(d, ".rafiki/skills"), "project-tier dir %q must not appear when hasExecutor is true", d)
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
	c := assert.NewCollecting(t)
	silenceStandaloneLogs(t)
	stdin := &countingCloser{}
	hook, fired := standaloneFatal(stdin)

	want := errors.New("turn panicked")
	hook(want)

	select {
	case got := <-fired:
		c.ErrorIs(got, want, "fired error")
	case <-time.After(5 * time.Second):
		t.Fatal("standaloneFatal never reported the fatal error; runAgent would exit 0 as if nothing happened")
	}
	c.Eq(1, stdin.count(), "stdin closed")

	// OnFatal is once-only by contract, but the send is into a size-1 buffer and
	// the close is on a real file: a second call must be a no-op either way.
	hook(errors.New("second"))
	c.Eq(1, stdin.count(), "stdin closed")
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
		got := bashRTKValue("")
		assert.NewCollecting(t).Eq("auto", got, "bashRTKValue(\"\") = %q, want auto", got)
	})

	// Env var only → use env var
	t.Run("env", func(t *testing.T) {
		t.Setenv("RAFIKI_BASH_RTK", "off")
		got := bashRTKValue("")
		assert.NewCollecting(t).Eq("off", got, "bashRTKValue(\"\") = %q, want off", got)
	})

	// Explicit flag beats env var
	t.Run("flag-beats-env", func(t *testing.T) {
		t.Setenv("RAFIKI_BASH_RTK", "off")
		got := bashRTKValue("on")
		assert.NewCollecting(t).Eq("on", got, "bashRTKValue(\"on\") = %q, want on (explicit flag must beat env)", got)
	})
}

// TestParseAgentFlagsBashRTK verifies the --bash-rtk flag is actually read
// by parseAgentFlags. This is the exact check that would have caught
// --record-requests before it shipped parsed-but-unread.
func TestParseAgentFlagsBashRTK(t *testing.T) {
	c := assert.NewCollecting(t)
	// Flag passed → f.bashRTK is set
	f, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest", "--bash-rtk", "on"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("on", f.bashRTK, "bashRTK")

	// Flag not passed → empty
	f2, err := parseAgentFlags([]string{"--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("", f2.bashRTK, "bashRTK")
}

// TestToolsWebValuePrecedence verifies the --tools-web precedence: an
// explicitly passed flag beats $RAFIKI_TOOLS_WEB beats the "off" default. The
// point of the flag is being able to force the toggle OFF even when the env var
// says on, so both directions are asserted, not just flag-wins-when-on.
func TestToolsWebValuePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        string
		flagVal    bool
		flagPassed bool
		want       bool
	}{
		{"default", "", false, false, false},
		{"env-only", "1", false, false, true},
		{"env set to something else", "0", false, false, false},
		{"flag on beats env unset", "", true, true, true},
		// The direction a bare bool cannot express without the passed signal.
		{"flag off beats env on", "1", false, true, false},
		{"flag on with env on", "1", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RAFIKI_TOOLS_WEB", tc.env)
			got := toolsWebValue(tc.flagVal, tc.flagPassed)
			assert.NewCollecting(t).Eq(tc.want, got, "toolsWebValue(%v, %v) with $RAFIKI_TOOLS_WEB=%q = %v, want", tc.flagVal, tc.flagPassed, tc.env, got)
		})
	}
}

// TestParseAgentFlagsToolsWeb verifies --tools-web is actually read by
// parseAgentFlags, the same regression class TestParseAgentFlagsBashRTK
// guards against (a flag registered but never read from the parsed struct),
// and that the passed/not-passed signal survives parsing.
func TestParseAgentFlagsToolsWeb(t *testing.T) {
	base := []string{"--model", "anthropic/sonnet-latest"}

	for _, tc := range []struct {
		name       string
		args       []string
		wantVal    bool
		wantPassed bool
	}{
		{"bare flag", []string{"--tools-web"}, true, true},
		{"explicit true", []string{"--tools-web=true"}, true, true},
		{"explicit false", []string{"--tools-web=false"}, false, true},
		{"absent", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			f, err := parseAgentFlags(append(append([]string{}, base...), tc.args...))
			c.Require().NoError(err, "parseAgentFlags")
			c.False(f.toolsWeb != tc.wantVal || f.toolsWebSet != tc.wantPassed, "toolsWeb=%v set=%v, want %v/%v", f.toolsWeb, f.toolsWebSet, tc.wantVal, tc.wantPassed)
		})
	}
}

// TestParseAgentFlagsToolsWebDoesNotEatNextFlag is why --tools-web is a bool
// rather than a string in --bash-rtk's shape. stdlib's flag package resolves
// `--strflag --model X` by taking "--model" as the value and leaving --model
// unset, reporting no error at all — so `rafikid agent --tools-web --model M`
// would have silently produced an empty model. A bool flag cannot consume the
// following argument, so --model still lands.
func TestParseAgentFlagsToolsWebDoesNotEatNextFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	f, err := parseAgentFlags([]string{"--tools-web", "--model", "anthropic/sonnet-latest"})
	c.Require().NoError(err, "parseAgentFlags")
	c.Eq("anthropic/sonnet-latest", f.model, "model")
	c.False(!f.toolsWeb || !f.toolsWebSet, "toolsWeb=%v set=%v, want true/true", f.toolsWeb, f.toolsWebSet)
}

// TestEffectiveLSPConfigPrecedence covers finding 15's extracted helper: an
// explicit --lsp-config must survive a missing file (so BuildRuntime raises
// the hard error), while a defaulted path that doesn't exist must be blanked
// (so a cwd with no lsp.json just runs without LSP tools, as before).
func TestEffectiveLSPConfigPrecedence(t *testing.T) {
	t.Run("explicit missing path is preserved", func(t *testing.T) {
		got := effectiveLSPConfig("/does/not/exist/lsp.json", t.TempDir())
		assert.NewCollecting(t).Eq("/does/not/exist/lsp.json", got, "effectiveLSPConfig")
	})

	t.Run("defaulted missing path is blanked", func(t *testing.T) {
		cwd := t.TempDir() // no .lsp.json written
		// Isolate from the dev machine's global lsp.json both via the env-var
		// path ($RAFIKI_LSP_CONFIG, checked first) and via the ConfigDir
		// fallback (XDG_CONFIG_HOME). Either alone could resolve to a real
		// file when the test runner's env carries defaults through rtk.
		t.Setenv("RAFIKI_LSP_CONFIG", filepath.Join(t.TempDir(), "nonexistent-lsp.json"))
		t.Setenv("XDG_CONFIG_HOME", cwd)
		got := effectiveLSPConfig("", cwd)
		assert.NewCollecting(t).Eq("", got, "effectiveLSPConfig(\"\", %q) = %q, want empty (defaulted path absent: skip LSP)", cwd, got)
	})

	t.Run("defaulted present path is kept", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cwd := t.TempDir()
		cwdCfg := filepath.Join(cwd, ".lsp.json")
		c.Require().NoError(os.WriteFile(cwdCfg, []byte(`{}`), 0o644))
		got := effectiveLSPConfig("", cwd)
		c.Eq(cwdCfg, got, "effectiveLSPConfig(\"\", %q) = %q, want", cwd, got)
	})

	t.Run("explicit existing path is kept", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cwd := t.TempDir()
		explicit := filepath.Join(t.TempDir(), "custom-lsp.json")
		c.Require().NoError(os.WriteFile(explicit, []byte(`{}`), 0o644))
		got := effectiveLSPConfig(explicit, cwd)
		c.Eq(explicit, got, "effectiveLSPConfig(%q, %q) = %q, want", explicit, cwd, got)
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
	c := assert.NewCollecting(t)
	// resolveModelDefaults/resolveAllowlistOption were added to this list after
	// the same class of drift recurred: runAgentWithFlags shipped without the
	// model-declared skills/MCP/context-files resolution that toRuntimeOptions
	// had, so `rafikid fundi` silently ignored every one of those alias fields.
	wantCalls := []string{
		"effectiveLSPConfig",
		"toolsWebValue",
		"resolveModelDefaults",
		"resolveAllowlistOption",
	}

	for file, fn := range map[string]string{
		"agent.go":         "runAgentWithFlags",
		"agent_runtime.go": "toRuntimeOptions",
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		c.Require().NoError(err, "parse %s", file)

		var body *ast.FuncDecl
		for _, decl := range parsed.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				body = fd
				break
			}
		}
		c.Require().NotNil(body, "%s: no func %s; this test needs updating alongside the rename", file, fn)

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
			c.False(!called[want], "%s: %s does not call %s; finding 15's duplicated resolution logic is back", file, fn, want)
		}
	}
}

func TestAssembleSkillDirsOmitsTheDefaultConfigDir(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "")
	got := assembleSkillDirs("/w", nil, true)
	assert.NewCollecting(t).Empty(got, "got")
}

func TestAssembleSkillDirsHonoursAnExplicitEnv(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "/opt/skills")
	got := assembleSkillDirs("/w", nil, true)
	assert.NewCollecting(t).False(len(got) != 1 || got[0] != "/opt/skills", "got %v, want [/opt/skills]", got)
}
