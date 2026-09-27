package fundi

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// isolateHome points $HOME at an empty temp directory and clears
// $RAFIKI_INSTRUCTIONS/$XDG_CONFIG_HOME so tests never pick up the real
// developer machine's own fundi instructions file (or a stray
// ~/.claude/CLAUDE.md, back when that was the source).
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RAFIKI_INSTRUCTIONS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
}

// captureSlog swaps the default slog.Logger for one writing into a
// *syncBuffer (see engine_test.go) for the duration of the test, returning
// an accessor for what was logged - so a test can assert a non-not-exist
// stat error was actually logged, not just silently mapped to "absent".
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	var b syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &b
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	c.NoError(os.WriteFile(path, []byte(content), 0o644))
}

func TestLoadContextFilesUserGlobal(t *testing.T) {
	c := assert.NewAborting(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // force the ~/.config fallback, ignoring any real value
	t.Setenv("RAFIKI_INSTRUCTIONS", "")
	mustWriteFile(t, filepath.Join(home, ".config", "rafiki", "instructions.md"), "GLOBAL_MARKER instructions")

	cwd := t.TempDir() // no git root, no local instruction files
	got, err := LoadContextFiles(cwd)
	c.NoError(err)
	c.StrContains(got, "GLOBAL_MARKER", "expected global instructions-file content, got")
}

// TestLoadContextFiles_UsesFundiInstructionsNotClaude locks down the point of
// this task: fundi's user-global instructions come from
// paths.InstructionsFile() ($RAFIKI_INSTRUCTIONS, else <ConfigDir>/instructions.md),
// never from Claude Code's own ~/.claude/CLAUDE.md.
func TestLoadContextFiles_UsesFundiInstructionsNotClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A Claude profile that must NOT be read.
	mustWriteFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "CLAUDE-PROFILE-MARKER")

	// fundi's own instructions, which must be read.
	inst := filepath.Join(t.TempDir(), "instructions.md")
	mustWriteFile(t, inst, "FUNDI-INSTRUCTIONS-MARKER")
	t.Setenv("RAFIKI_INSTRUCTIONS", inst)

	got, err := LoadContextFiles(t.TempDir())
	c.Require().NoError(err)
	c.StrContains(got, "FUNDI-INSTRUCTIONS-MARKER", "did not load $RAFIKI_INSTRUCTIONS")
	c.NotStrContains(got, "CLAUDE-PROFILE-MARKER", "read ~/.claude/CLAUDE.md; fundi must not read its config from Claude's directory")
}

// TestLoadContextFiles_MissingInstructionsIsNotAnError covers the "most
// installs have no global instructions file yet" case: a $RAFIKI_INSTRUCTIONS
// pointing at a nonexistent path must be skipped silently, not returned as
// an error.
func TestLoadContextFiles_MissingInstructionsIsNotAnError(t *testing.T) {
	t.Setenv("RAFIKI_INSTRUCTIONS", filepath.Join(t.TempDir(), "absent.md"))
	_, err := LoadContextFiles(t.TempDir())
	assert.NewAborting(t).NoError(err, "missing instructions file must be skipped silently, got")
}

// TestLoadContextFilesNestedGitRootAndInclude covers the brief's primary
// scenario: a nested git root whose CLAUDE.md @-includes another file, and a
// deeper cwd with its own AGENTS.md. Both must appear, in order (git root
// before cwd), and the include must be inlined rather than left as a literal
// @-line.
func TestLoadContextFilesNestedGitRootAndInclude(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)

	root := t.TempDir()
	c.NoError(os.Mkdir(filepath.Join(root, ".git"), 0o755))
	mustWriteFile(t, filepath.Join(root, "CLAUDE.md"), "ROOT_MARKER instructions\n@docs/extra.md\n")
	mustWriteFile(t, filepath.Join(root, "docs", "extra.md"), "INCLUDED_MARKER content")

	cwd := filepath.Join(root, "deep", "sub", "dir")
	mustWriteFile(t, filepath.Join(cwd, "AGENTS.md"), "CWD_MARKER agent instructions")

	got, err := LoadContextFiles(cwd)
	c.NoError(err)

	for _, marker := range []string{"ROOT_MARKER", "INCLUDED_MARKER", "CWD_MARKER"} {
		c.StrContains(got, marker, "expected")
	}
	// The raw @-include line must not survive verbatim - it should have been
	// replaced by the included content.
	c.NotStrContains(got, "@docs/extra.md", "include line was not inlined")
	// git root content precedes cwd content (cache-stability ordering).
	c.LessOrEqual(strings.Index(got, "CWD_MARKER"), strings.Index(got, "ROOT_MARKER"), "expected root content before cwd content, got %q", got)
}

// TestLoadContextFilesDedupWhenCwdIsGitRoot covers the dedup rule: when cwd
// IS the git root, its CLAUDE.md must be emitted exactly once, not twice.
func TestLoadContextFilesDedupWhenCwdIsGitRoot(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)

	root := t.TempDir()
	c.NoError(os.Mkdir(filepath.Join(root, ".git"), 0o755))
	mustWriteFile(t, filepath.Join(root, "CLAUDE.md"), "ONLY_ONCE_MARKER")

	got, err := LoadContextFiles(root)
	c.NoError(err)
	n := strings.Count(got, "ONLY_ONCE_MARKER")
	c.Eq(1, n, "expected ONLY_ONCE_MARKER exactly once, got %d in %q", n, got)
}

// TestLoadContextFilesCycleTerminates is the brief's named scenario:
// a.md @-> b.md @-> a.md must terminate rather than hang or stack overflow,
// and produce the missing/cycle marker.
func TestLoadContextFilesCycleTerminates(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "@a.md")
	mustWriteFile(t, filepath.Join(cwd, "a.md"), "@b.md")
	mustWriteFile(t, filepath.Join(cwd, "b.md"), "@a.md")

	got, err := LoadContextFiles(cwd)
	c.NoError(err)
	c.StrContains(got, "[missing include:", "expected a missing/cycle marker, got")
}

// TestLoadContextFilesMissingInclude covers a single dangling @-reference: it
// must become the literal marker, not an error.
func TestLoadContextFilesMissingInclude(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "before\n@nope.md\nafter")

	got, err := LoadContextFiles(cwd)
	c.NoError(err)
	c.StrContains(got, "[missing include: nope.md]", "expected missing-include marker, got")
}

// TestLoadContextFilesDepthCapTerminates builds a long include chain (well
// past the depth-5 cap) with no cycle at all, to prove the cap itself - not
// just cycle detection - bounds recursion. The deepest file's content must
// not surface, and a marker must appear instead.
func TestLoadContextFilesDepthCapTerminates(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)

	cwd := t.TempDir()
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "@chain0.md")
	const chainLen = 10
	for i := 0; i < chainLen; i++ {
		mustWriteFile(t, filepath.Join(cwd, "chain"+strconv.Itoa(i)+".md"), "@chain"+strconv.Itoa(i+1)+".md")
	}
	mustWriteFile(t, filepath.Join(cwd, "chain"+strconv.Itoa(chainLen)+".md"), "UNREACHABLE_LEAF_MARKER")

	got, err := LoadContextFiles(cwd)
	c.NoError(err)
	c.NotStrContains(got, "UNREACHABLE_LEAF_MARKER", "depth cap did not bound recursion, leaf content leaked")
	c.StrContains(got, "[missing include:", "expected depth-cap marker, got")
}

// TestLoadContextFilesLogsNonNotExistStatError covers loadInstructionFile's
// doc comment ("could not be read for any other reason ... logged, not
// silently dropped"): a stat error other than "does not exist" - here,
// ENOTDIR from a path whose parent component is a regular file, not a
// directory, which is portable and doesn't depend on running as
// non-root - must be logged, not just mapped to the empty-string "absent"
// result used for the ordinary missing-file case.
func TestLoadContextFilesLogsNonNotExistStatError(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)
	logged := captureSlog(t)

	cwd := t.TempDir()
	// A regular file where CLAUDE.md's parent directory would need to be:
	// stat-ing "notadir/CLAUDE.md" fails with ENOTDIR, not ENOENT.
	notADir := filepath.Join(cwd, "notadir")
	c.NoError(os.WriteFile(notADir, []byte("x"), 0o644))

	got, err := LoadContextFiles(filepath.Join(notADir, "sub"))
	c.NoError(err)
	c.Eq("", got, "expected no content from an unreadable path, got")
	c.StrContains(logged.String(), "failed to stat instruction file", "expected the stat error to be logged, got")
}

// TestLoadContextFilesLogsNonNotExistIncludeStatError is
// TestLoadContextFilesLogsNonNotExistStatError's counterpart for
// resolveInclude's own os.Stat call: an @-include target whose parent path
// component is a regular file must log the stat error (not just emit the
// silent missing-include marker) - matching the analogous top-level
// instruction file case above.
func TestLoadContextFilesLogsNonNotExistIncludeStatError(t *testing.T) {
	c := assert.NewAborting(t)
	isolateHome(t)
	logged := captureSlog(t)

	cwd := t.TempDir()
	notADir := filepath.Join(cwd, "notadir")
	c.NoError(os.WriteFile(notADir, []byte("x"), 0o644))
	mustWriteFile(t, filepath.Join(cwd, "CLAUDE.md"), "before\n@notadir/CLAUDE.md\nafter")

	got, err := LoadContextFiles(cwd)
	c.NoError(err)
	c.StrContains(got, "[missing include: notadir/CLAUDE.md]", "expected missing-include marker for the unreadable target, got")
	c.StrContains(logged.String(), "failed to stat include target", "expected the stat error to be logged, got")
}

func TestTruncateContextFiles_NoCapReturnsUnchanged(t *testing.T) {
	content := strings.Repeat("x", 10000)
	assert.NewCollecting(t).Eq(content, truncateContextFiles(content, 0), "budgetTokens=0 must return content unchanged")
}

func TestTruncateContextFiles_UnderBudgetReturnsUnchanged(t *testing.T) {
	content := "short content\nsecond line"
	assert.NewCollecting(t).Eq(content, truncateContextFiles(content, 1000), "content under budget must return unchanged, got")
}

func TestTruncateContextFiles_OverBudgetCutsAtNewlineAndMarks(t *testing.T) {
	c := assert.NewCollecting(t)
	// estimatedCharsPerToken=4, so budget=10 tokens => 40 byte budget.
	content := "0123456789\n0123456789\n0123456789\n0123456789\n"
	got := truncateContextFiles(content, 10)
	c.NotStrContains(got, "0123456789\n0123456789\n0123456789\n0123456789", "expected truncation, but full content survived")
	if !strings.HasPrefix(got, "0123456789\n0123456789\n0123456789") {
		t.Errorf("expected the kept prefix to end at a newline boundary within budget, got %q", got)
	}
	c.StrContains(got, "truncated to fit this model's context budget", "expected a truncation marker, got")
}
