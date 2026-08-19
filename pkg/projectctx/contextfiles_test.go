package projectctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The project tier is CLAUDE.md and AGENTS.md at the git root and at cwd. The
// user's global instructions file is NOT part of it: that belongs to whoever
// runs the agent loop, which is a different machine from the workspace.
func TestLoadProjectContextReadsCwdFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("project rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProjectContext(dir)
	if err != nil {
		t.Fatalf("LoadProjectContext: %v", err)
	}
	if !strings.Contains(got, "project rules") {
		t.Errorf("got %q, want it to contain the CLAUDE.md content", got)
	}
}

// A directory with neither file is the common case and must not be an error.
func TestLoadProjectContextEmptyIsNotAnError(t *testing.T) {
	got, err := LoadProjectContext(t.TempDir())
	if err != nil {
		t.Fatalf("LoadProjectContext: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// Includes are expanded relative to the including file, on this side of the
// boundary. They must not travel to the daemon as unexpanded @refs: the daemon
// cannot resolve a path on the executor's filesystem.
func TestLoadProjectContextExpandsIncludes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extra.md"), []byte("included body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("head\n@extra.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProjectContext(dir)
	if err != nil {
		t.Fatalf("LoadProjectContext: %v", err)
	}
	if !strings.Contains(got, "included body") {
		t.Errorf("got %q, want the include expanded", got)
	}
}
