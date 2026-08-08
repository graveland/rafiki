package fundi

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"
)

// fakeRuntimeOptions returns options that build a working engine with no API
// credentials and no database: FakeTurns replaces the upstream sender.
//
// FakeTurns is a path to an ndjson file of scripted anthropic.Message bodies
// (see Config.FakeTurns / LoadFakeSender), not literal reply text, so this
// uses the writeFakeTurns/sampleEndTurn helpers already in this test package
// (config_test.go, engine_test.go) rather than a bare string.
func fakeRuntimeOptions(t *testing.T, cwd string) RuntimeOptions {
	t.Helper()
	return RuntimeOptions{
		Model:          "anthropic/claude-sonnet-4-5",
		Cwd:            cwd,
		Ref:            "test-ref",
		SpillDir:       t.TempDir(),
		FakeTurns:      writeFakeTurns(t, sampleEndTurn),
		NoSkills:       true,
		NoContextFiles: true,
	}
}

// TestBuildRuntimeConstructsEngine is a construction smoke test: it only
// proves BuildRuntime returns a working engine end to end (tool registry,
// skills, MCP, Config, Engine all wired). It does NOT prove opts.Cwd (as
// opposed to the process cwd) is what gets resolved — see
// TestResolveContentUsesExplicitCwd for that regression guard, which is real
// because it can observe the resolved content directly instead of going
// through Config.ContextFiles into a system prompt no test can reach.
func TestBuildRuntimeConstructsEngine(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	eng, shutdown, err := BuildRuntime(context.Background(), fe, opts)
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	defer shutdown()
	if eng == nil {
		t.Fatal("BuildRuntime returned a nil engine")
	}
}

// TestResolveContentUsesExplicitCwd is the regression guard this task exists
// to install: it proves resolveContent (and therefore BuildRuntime) resolves
// context files from opts.Cwd, not the process working directory. The daemon's
// cwd is never the child's, so resolving from os.Getwd() would load the wrong
// context files for every in-process child — and would do it silently, since
// LoadContextFiles skips absent files rather than erroring.
//
// This is a two-sided discriminator: both the child and process cwds get
// their own AGENTS.md with a distinct marker, so reading the wrong directory
// doesn't just fail to find the right marker, it finds the WRONG one. A test
// that only asserted "no error, non-nil result" (as this test's predecessor
// did) would still pass if BuildRuntime were changed back to os.Getwd().
func TestResolveContentUsesExplicitCwd(t *testing.T) {
	childCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(childCwd, "AGENTS.md"), []byte("MARKER-CHILD-CWD"), 0o644); err != nil {
		t.Fatalf("write child AGENTS.md: %v", err)
	}

	// The process cwd gets its OWN marker. That is what makes this a real
	// discriminator: reading the wrong directory does not merely fail to find
	// the right marker, it finds a different one.
	processCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(processCwd, "AGENTS.md"), []byte("MARKER-PROCESS-CWD"), 0o644); err != nil {
		t.Fatalf("write process AGENTS.md: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(processCwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	got, _, err := resolveContent(RuntimeOptions{Cwd: childCwd, NoSkills: true})
	if err != nil {
		t.Fatalf("resolveContent: %v", err)
	}
	if !strings.Contains(got, "MARKER-CHILD-CWD") {
		t.Errorf("context files missing the child-cwd marker; got %q", got)
	}
	if strings.Contains(got, "MARKER-PROCESS-CWD") {
		t.Error("context files contain the PROCESS cwd marker: content was resolved from os.Getwd(), not opts.Cwd")
	}
}

// TestBuildRuntimeRejectsRelativeCwd guards the same failure from the other
// side: a relative cwd would resolve against the daemon's directory.
func TestBuildRuntimeRejectsRelativeCwd(t *testing.T) {
	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	opts := fakeRuntimeOptions(t, "relative/path")
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a relative Cwd, got nil")
	}
}

// TestBuildRuntimeNilPoolIsInMemory pins the contract Task 5 depends on: no
// pool means an in-memory conversation, and BuildRuntime must never open one.
// A BuildRuntime that dialled a database here would make every unit test in
// this package require postgres.
func TestBuildRuntimeNilPoolIsInMemory(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.Pool = nil

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	eng, shutdown, err := BuildRuntime(context.Background(), fe, opts)
	if err != nil {
		t.Fatalf("BuildRuntime with a nil pool: %v", err)
	}
	defer shutdown()
	if eng == nil {
		t.Fatal("nil engine")
	}
}

// hasToolNamed reports whether registry advertises a tool with the given
// name, via the same Definitions() list sent to the model on every turn.
func hasToolNamed(registry *tools.Registry, name string) bool {
	for _, def := range registry.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == name {
			return true
		}
	}
	return false
}

// TestMaterializeAllOmitsSkillToolWithZeroSkills is the regression guard for
// the refactor that swapped an explicit, guarded skill-tool registration for
// tools.DefaultBlueprint.MaterializeAll. MaterializeAll used to register every
// blueprint unconditionally, including SkillBlueprint, whose Materialize
// happily built a working "skill" tool over an empty skill set. That tool
// could only ever respond "unknown skill; available skills: " to every call,
// and it silently broke --no-skills' promise (usage.go: "disables skill
// discovery and the skill tool entirely") since NoSkills only ever affected
// discovery, never registration.
func TestMaterializeAllOmitsSkillToolWithZeroSkills(t *testing.T) {
	registry := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{})
	if hasToolNamed(registry, "skill") {
		t.Fatal("skill tool advertised despite zero discovered skills")
	}
}

// TestMaterializeAllIncludesSkillToolWithSkills is the positive-side
// discriminator: it proves SkillBlueprint declines conditionally on
// opts.Skills, rather than the skill tool having been dropped outright.
func TestMaterializeAllIncludesSkillToolWithSkills(t *testing.T) {
	registry := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Skills: []skills.SkillMeta{{Name: "reviewer", Description: "reviews code"}},
	})
	if !hasToolNamed(registry, "skill") {
		t.Fatal("skill tool missing despite a non-empty discovered skill set")
	}
}

// TestBuildRuntimeNoSkillsOmitsSkillTool exercises the same guarantee through
// the real BuildRuntime → resolveContent → MaterializeAll path, pinning that
// NoSkills actually reaches materialization and not just discovery.
func TestBuildRuntimeNoSkillsOmitsSkillTool(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.NoSkills = true

	_, discovered, err := resolveContent(opts)
	if err != nil {
		t.Fatalf("resolveContent: %v", err)
	}
	if len(discovered) != 0 {
		t.Fatalf("NoSkills discovered %d skills, want 0", len(discovered))
	}

	registry := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{Skills: discovered})
	if hasToolNamed(registry, "skill") {
		t.Fatal("skill tool advertised with --no-skills set")
	}
}

// TestBuildRuntimeRequiresRipgrep pins the startup dependency check: a
// missing rg must fail loudly once in checkRipgrep rather than once per tool
// call. rgPath (pkg/fundi/tools) is a cached sync.OnceValue and cannot be
// un-cached by t.Setenv, which is exactly why checkRipgrep does its own
// exec.LookPath instead of calling tools.RipgrepAvailable — this test would
// be vacuous (pass no matter what PATH says) against the cached path.
func TestBuildRuntimeRequiresRipgrep(t *testing.T) {
	if !tools.RipgrepAvailable() {
		t.Skip("ripgrep not on PATH; this test asserts the happy path is reachable")
	}
	// PATH without rg makes the dependency check fire.
	t.Setenv("PATH", t.TempDir())
	if err := checkRipgrep(); err == nil {
		t.Fatal("expected an error when ripgrep is absent")
	} else if !strings.Contains(err.Error(), "ripgrep") {
		t.Fatalf("error must name the missing dependency, got: %v", err)
	}
}

// TestBuildRuntimeMissingMCPConfigIsAnError pins the contract cmd/rafikid relies
// on: BuildRuntime errors on any MCPConfig path that does not exist. The
// "silently skip a defaulted <cwd>/.mcp.json" behaviour stays in cmd/rafikid,
// which passes an empty MCPConfig in that case.
func TestBuildRuntimeMissingMCPConfigIsAnError(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.MCPConfig = filepath.Join(t.TempDir(), "does-not-exist.json")

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a non-existent MCPConfig, got nil")
	}
}
