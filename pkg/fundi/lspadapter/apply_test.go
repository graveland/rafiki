package lspadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

func edit(startLine, startCol, endLine, endCol int, newText string) lsp.TextEdit {
	return lsp.TextEdit{
		Range: lsp.Range{
			Start: lsp.Position{Line: startLine, Character: startCol},
			End:   lsp.Position{Line: endLine, Character: endCol},
		},
		NewText: newText,
	}
}

// TestApplyTextEditsUTF16Columns is the regression test for the corruption
// bug. LSP columns are UTF-16 code units by default, and the old code added
// Position.Character straight to a byte index, so a line with CJK before the
// edited column was cut mid-rune: replacing `foo` in `x := "日本語" + foo`
// produced `x := "日本\xe8BAR + foo` — invalid UTF-8, and the wrong span.
func TestApplyTextEditsUTF16Columns(t *testing.T) {
	c := assert.NewAborting(t)
	src := "x := \"日本語\" + foo\n"
	// In UTF-16 units: `x := "` is 6, the three CJK runes are 1 unit each,
	// `" + ` is 4 -> foo starts at 13 and ends at 16.
	got, err := applyTextEdits([]byte(src), []lsp.TextEdit{edit(0, 13, 0, 16, "BAR")}, lsp.PositionEncodingUTF16)
	c.NoError(err)
	want := "x := \"日本語\" + BAR\n"
	c.Eq(want, string(got), "utf-16 column handling is wrong:\n got %q\nwant", got)
	c.True(utf8.Valid(got), "edit produced invalid UTF-8: a rune was cut in half")
}

// TestApplyTextEditsUTF8Columns covers the other negotiated encoding, where
// a column really is a byte offset.
func TestApplyTextEditsUTF8Columns(t *testing.T) {
	c := assert.NewAborting(t)
	src := "x := \"日本語\" + foo\n"
	start := strings.Index(src, "foo")
	got, err := applyTextEdits([]byte(src), []lsp.TextEdit{edit(0, start, 0, start+3, "BAR")}, lsp.PositionEncodingUTF8)
	c.NoError(err)
	c.Eq("x := \"日本語\" + BAR\n", string(got), "utf-8 column handling is wrong:\n got %q\nwant", got)
}

// TestApplyTextEditsUnsortedInput pins that the apply order does not depend
// on the order the server happened to send. All ranges refer to the original
// document, so they must be applied last-first; iterating the slice backwards
// assumed an ascending array the spec does not guarantee. With these two
// edits in descending order the old code produced "aaa QUUX barQUUXo zzz".
func TestApplyTextEditsUnsortedInput(t *testing.T) {
	c := assert.NewAborting(t)
	src := "aaa foo bar foo zzz\n"
	edits := []lsp.TextEdit{
		edit(0, 12, 0, 15, "QUUX"), // second occurrence, sent FIRST
		edit(0, 4, 0, 7, "QUUX"),   // first occurrence, sent SECOND
	}
	got, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16)
	c.NoError(err)
	c.Eq("aaa QUUX bar QUUX zzz\n", string(got), "apply order depends on server ordering:\n got %q\nwant", got)
}

func TestApplyTextEditsMultiLine(t *testing.T) {
	c := assert.NewAborting(t)
	src := "package main\n\nfunc Add(a, b int) int { return a + b }\n\nvar _ = Add\n"
	edits := []lsp.TextEdit{
		edit(2, 5, 2, 8, "Sum"),
		edit(4, 8, 4, 11, "Sum"),
	}
	got, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16)
	c.NoError(err)
	want := "package main\n\nfunc Sum(a, b int) int { return a + b }\n\nvar _ = Sum\n"
	c.Eq(want, string(got), "multi-line rename wrong:\n got %q\nwant", got)
}

func TestApplyTextEditsRejectsOverlap(t *testing.T) {
	src := "aaa bbb ccc\n"
	edits := []lsp.TextEdit{edit(0, 0, 0, 7, "X"), edit(0, 4, 0, 11, "Y")}
	_, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16)
	assert.NewAborting(t).Error(err, "overlapping edits must be refused, not silently mis-applied")
}

// TestApplyWorkspaceEditIsAtomic pins that a failure part-way through leaves
// NOTHING written. The old version wrote each file as it iterated a Go map —
// random order — so a failure on the 4th of 7 files left a nondeterministic
// subset rewritten: a workspace that no longer compiles and cannot be
// reproduced.
func TestApplyWorkspaceEditIsAtomic(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "a.go")
	bad := filepath.Join(dir, "b.go")
	const goodSrc = "package a\n\nvar Foo = 1\n"
	const badSrc = "package b\n"
	c.NoError(os.WriteFile(good, []byte(goodSrc), 0o644))
	c.NoError(os.WriteFile(bad, []byte(badSrc), 0o644))

	byFile := map[string][]lsp.TextEdit{
		good: {edit(2, 4, 2, 7, "Bar")},
		bad:  {edit(99, 0, 99, 5, "nope")}, // range past EOF -> invalid
	}
	tracker := tools.NewFileTracker()
	if _, err := applyWorkspaceEdit(byFile, lsp.PositionEncodingUTF16, tracker); err == nil {
		t.Fatal("expected the invalid range to fail the whole edit")
	}

	// The valid file must be untouched: all-or-nothing.
	after, err := os.ReadFile(good)
	c.NoError(err)
	c.Eq(goodSrc, string(after), "a failed workspace edit left %s modified:\n%q", good, after)
}

// TestApplyWorkspaceEditUnwritableTargetLeavesNothingModified is the
// required regression test for the atomicity fix: staging computed every
// edit successfully (all ranges valid), but the WRITE for one target fails
// (its directory is read-only, so even creating the ".rename-tmp" sibling
// fails). Every original file must be untouched, and no stray temp file may
// survive -- both properties the old "write each file in a loop" version
// could not offer, since it wrote directly to the real target and left
// whatever succeeded before the failure in place.
func TestApplyWorkspaceEditUnwritableTargetLeavesNothingModified(t *testing.T) {
	c := assert.NewCollecting(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions don't block writes")
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "a.go")
	const goodSrc = "package a\n\nvar Foo = 1\n"
	c.Require().NoError(os.WriteFile(good, []byte(goodSrc), 0o644))

	roDir := filepath.Join(dir, "readonly")
	c.Require().NoError(os.Mkdir(roDir, 0o755))
	bad := filepath.Join(roDir, "b.go")
	const badSrc = "package b\n\nvar Foo = 1\n"
	c.Require().NoError(os.WriteFile(bad, []byte(badSrc), 0o644))
	// Deny writes to the directory so even creating "b.go.rename-tmp" fails
	// -- the target file's own mode is irrelevant to creating a new sibling.
	c.Require().NoError(os.Chmod(roDir, 0o555))
	// Restore write permission so t.TempDir()'s cleanup can remove the dir.
	t.Cleanup(func() {
		c.NoError(os.Chmod(roDir, 0o755), "restore %s mode", roDir)
	})

	byFile := map[string][]lsp.TextEdit{
		good: {edit(2, 4, 2, 7, "Bar")},
		bad:  {edit(2, 4, 2, 7, "Bar")},
	}
	tracker := tools.NewFileTracker()
	if _, err := applyWorkspaceEdit(byFile, lsp.PositionEncodingUTF16, tracker); err == nil {
		t.Fatal("expected the unwritable target to fail the whole edit")
	}

	afterGood, err := os.ReadFile(good)
	c.Require().NoError(err)
	c.Require().Eq(goodSrc, string(afterGood), "a failed workspace edit left %s modified:\n%q", good, afterGood)

	afterBad, err := os.ReadFile(bad)
	c.Require().NoError(err)
	c.Require().Eq(badSrc, string(afterBad), "a failed workspace edit left %s modified:\n%q", bad, afterBad)

	// No stray ".rename-tmp" files anywhere in dir.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		c.False(strings.HasSuffix(path, ".rename-tmp"), "stray temp file left behind: %s", path)
		return nil
	})
}

// TestApplyWorkspaceEditPreservesFileMode pins that the temp-file-then-
// rename path (added for atomicity) keeps writing files with their
// original mode rather than a hardcoded default.
func TestApplyWorkspaceEditPreservesFileMode(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "exec.go")
	c.Require().NoError(os.WriteFile(p, []byte("package x\n\nvar Foo = 1\n"), 0o755))

	byFile := map[string][]lsp.TextEdit{p: {edit(2, 4, 2, 7, "Bar")}}
	if _, err := applyWorkspaceEdit(byFile, lsp.PositionEncodingUTF16, tools.NewFileTracker()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(p)
	c.Require().NoError(err)
	c.Eq(0o755, info.Mode().Perm(), "mode")
}

func TestApplyWorkspaceEditWritesEveryFile(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	var paths []string
	byFile := map[string][]lsp.TextEdit{}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		p := filepath.Join(dir, name)
		c.NoError(os.WriteFile(p, []byte("package x\n\nvar Foo = 1\n"), 0o644))
		paths = append(paths, p)
		byFile[p] = []lsp.TextEdit{edit(2, 4, 2, 7, "Bar")}
	}

	modified, err := applyWorkspaceEdit(byFile, lsp.PositionEncodingUTF16, tools.NewFileTracker())
	c.NoError(err)
	c.Len(modified, len(paths), "modified %d files, want", len(modified))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		c.NoError(err)
		c.StrContains(string(b), "var Bar = 1", "%s was not edited: %q", p, b)
	}
}
