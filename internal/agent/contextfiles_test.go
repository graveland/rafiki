package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// isolateHome points $HOME at an empty temp directory so tests never pick up
// the real developer machine's ~/.claude/CLAUDE.md.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadContextFilesUserGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWriteFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "GLOBAL_MARKER instructions")

	cwd := t.TempDir() // no git root, no local instruction files
	got, err := LoadContextFiles(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "GLOBAL_MARKER") {
		t.Fatalf("expected global CLAUDE.md content, got %q", got)
	}
}

// TestLoadContextFilesNestedGitRootAndInclude covers the brief's primary
// scenario: a nested git root whose CLAUDE.md @-includes another file, and a
// deeper cwd with its own AGENTS.md. Both must appear, in order (git root
// before cwd), and the include must be inlined rather than left as a literal
// @-line.
func TestLoadContextFilesNestedGitRootAndInclude(t *testing.T) {
	isolateHome(t)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "CLAUDE.md"), "ROOT_MARKER instructions\n@docs/extra.md\n")
	mustWriteFile(t, filepath.Join(root, "docs", "extra.md"), "INCLUDED_MARKER content")

	cwd := filepath.Join(root, "deep", "sub", "dir")
	mustWriteFile(t, filepath.Join(cwd, "AGENTS.md"), "CWD_MARKER agent instructions")

	got, err := LoadContextFiles(cwd)
	if err != nil {
		t.Fatal(err)
	}

	for _, marker := range []string{"ROOT_MARKER", "INCLUDED_MARKER", "CWD_MARKER"} {
		if !strings.Contains(got, marker) {
			t.Fatalf("expected %s in output, got %q", marker, got)
		}
	}
	// The raw @-include line must not survive verbatim - it should have been
	// replaced by the included content.
	if strings.Contains(got, "@docs/extra.md") {
		t.Fatalf("include line was not inlined: %q", got)
	}
	// git root content precedes cwd content (cache-stability ordering).
	if strings.Index(got, "ROOT_MARKER") > strings.Index(got, "CWD_MARKER") {
		t.Fatalf("expected root content before cwd content, got %q", got)
	}
}

// TestLoadContextFilesDedupWhenCwdIsGitRoot covers the dedup rule: when cwd
// IS the git root, its CLAUDE.md must be emitted exactly once, not twice.
func TestLoadContextFilesDedupWhenCwdIsGitRoot(t *testing.T) {
	isolateHome(t)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "CLAUDE.md"), "ONLY_ONCE_MARKER")

	got, err := LoadContextFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "ONLY_ONCE_MARKER"); n != 1 {
		t.Fatalf("expected ONLY_ONCE_MARKER exactly once, got %d in %q", n, got)
	}
}

// TestLoadContextFilesCycleTerminates is the brief's named scenario:
// a.md @-> b.md @-> a.md must terminate rather than hang or stack overflow,
// and produce the missing/cycle marker.
func TestLoadContextFilesCycleTerminates(t *testing.T) {
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "@a.md")
	mustWriteFile(t, filepath.Join(cwd, "a.md"), "@b.md")
	mustWriteFile(t, filepath.Join(cwd, "b.md"), "@a.md")

	got, err := LoadContextFiles(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[missing include:") {
		t.Fatalf("expected a missing/cycle marker, got %q", got)
	}
}

// TestLoadContextFilesMissingInclude covers a single dangling @-reference: it
// must become the literal marker, not an error.
func TestLoadContextFilesMissingInclude(t *testing.T) {
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "before\n@nope.md\nafter")

	got, err := LoadContextFiles(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[missing include: nope.md]") {
		t.Fatalf("expected missing-include marker, got %q", got)
	}
}

// TestLoadContextFilesDepthCapTerminates builds a long include chain (well
// past the depth-5 cap) with no cycle at all, to prove the cap itself - not
// just cycle detection - bounds recursion. The deepest file's content must
// not surface, and a marker must appear instead.
func TestLoadContextFilesDepthCapTerminates(t *testing.T) {
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "@chain0.md")
	const chainLen = 10
	for i := 0; i < chainLen; i++ {
		mustWriteFile(t, filepath.Join(cwd, "chain"+strconv.Itoa(i)+".md"), "@chain"+strconv.Itoa(i+1)+".md")
	}
	mustWriteFile(t, filepath.Join(cwd, "chain"+strconv.Itoa(chainLen)+".md"), "UNREACHABLE_LEAF_MARKER")

	got, err := LoadContextFiles(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "UNREACHABLE_LEAF_MARKER") {
		t.Fatalf("depth cap did not bound recursion, leaf content leaked: %q", got)
	}
	if !strings.Contains(got, "[missing include:") {
		t.Fatalf("expected depth-cap marker, got %q", got)
	}
}
