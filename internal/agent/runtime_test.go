package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestBuildRuntimeUsesExplicitCwd proves BuildRuntime resolves from opts.Cwd,
// not the process working directory. The daemon's cwd is never the child's, so
// a BuildRuntime that called os.Getwd would load the wrong context files and
// skills for every in-process child — and would do it silently.
func TestBuildRuntimeUsesExplicitCwd(t *testing.T) {
	childCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(childCwd, "AGENTS.md"), []byte("MARKER-CHILD-CWD"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Run from somewhere else so os.Getwd() cannot accidentally pass.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	opts := fakeRuntimeOptions(t, childCwd)
	opts.NoContextFiles = false // the point of this test is that AGENTS.md is found

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

// TestBuildRuntimeRejectsRelativeCwd guards the same failure from the other
// side: a relative cwd would resolve against the daemon's directory.
func TestBuildRuntimeRejectsRelativeCwd(t *testing.T) {
	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	opts := fakeRuntimeOptions(t, "relative/path")
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a relative Cwd, got nil")
	}
}

// TestBuildRuntimeMissingMCPConfigIsAnError pins the contract cmd/fundid relies
// on: BuildRuntime errors on any MCPConfig path that does not exist. The
// "silently skip a defaulted <cwd>/.mcp.json" behaviour stays in cmd/fundid,
// which passes an empty MCPConfig in that case.
func TestBuildRuntimeMissingMCPConfigIsAnError(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.MCPConfig = filepath.Join(t.TempDir(), "does-not-exist.json")

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a non-existent MCPConfig, got nil")
	}
}
