package fundi

import (
	"path/filepath"
	"strings"
	"testing"
)

// A non-nil project override must win over reading cwd: the override is what
// the executor returned for the workspace, and cwd is a path on a machine the
// daemon frequently cannot even see.
func TestLoadContextFilesProjectOverrideWins(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "DAEMON_CWD_MARKER")

	override := "EXECUTOR_MARKER"
	got, err := loadContextFiles(cwd, &override, 0)
	if err != nil {
		t.Fatalf("loadContextFiles: %v", err)
	}
	if !strings.Contains(got, "EXECUTOR_MARKER") {
		t.Errorf("got %q, want the override content", got)
	}
	if strings.Contains(got, "DAEMON_CWD_MARKER") {
		t.Error("read the daemon's cwd despite a project override")
	}
}

// The empty string is a real answer, not a fallback signal. An executor-backed
// child whose workspace has no instruction files must get global-only context,
// NOT the daemon's CLAUDE.md at a cwd path that does not exist there — reading
// it back is the exact bug this split exists to prevent.
func TestLoadContextFilesEmptyProjectOverrideIsNotFallback(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "DAEMON_CWD_MARKER")

	empty := ""
	got, err := loadContextFiles(cwd, &empty, 0)
	if err != nil {
		t.Fatalf("loadContextFiles: %v", err)
	}
	if strings.Contains(got, "DAEMON_CWD_MARKER") {
		t.Errorf("empty project override fell back to the daemon's cwd; got %q", got)
	}
}

// A nil override keeps today's behaviour: the project tier is read from cwd.
func TestLoadContextFilesNilOverrideReadsCwd(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "CWD_MARKER")

	got, err := loadContextFiles(cwd, nil, 0)
	if err != nil {
		t.Fatalf("loadContextFiles: %v", err)
	}
	if !strings.Contains(got, "CWD_MARKER") {
		t.Errorf("got %q, want the cwd content", got)
	}
}
