package fundi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
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
	src := "x := \"日本語\" + foo\n"
	// In UTF-16 units: `x := "` is 6, the three CJK runes are 1 unit each,
	// `" + ` is 4 -> foo starts at 13 and ends at 16.
	got, err := applyTextEdits([]byte(src), []lsp.TextEdit{edit(0, 13, 0, 16, "BAR")}, lsp.PositionEncodingUTF16)
	if err != nil {
		t.Fatal(err)
	}
	want := "x := \"日本語\" + BAR\n"
	if string(got) != want {
		t.Fatalf("utf-16 column handling is wrong:\n got %q\nwant %q", got, want)
	}
	if !utf8.Valid(got) {
		t.Fatal("edit produced invalid UTF-8: a rune was cut in half")
	}
}

// TestApplyTextEditsUTF8Columns covers the other negotiated encoding, where
// a column really is a byte offset.
func TestApplyTextEditsUTF8Columns(t *testing.T) {
	src := "x := \"日本語\" + foo\n"
	start := strings.Index(src, "foo")
	got, err := applyTextEdits([]byte(src), []lsp.TextEdit{edit(0, start, 0, start+3, "BAR")}, lsp.PositionEncodingUTF8)
	if err != nil {
		t.Fatal(err)
	}
	if want := "x := \"日本語\" + BAR\n"; string(got) != want {
		t.Fatalf("utf-8 column handling is wrong:\n got %q\nwant %q", got, want)
	}
}

// TestApplyTextEditsUnsortedInput pins that the apply order does not depend
// on the order the server happened to send. All ranges refer to the original
// document, so they must be applied last-first; iterating the slice backwards
// assumed an ascending array the spec does not guarantee. With these two
// edits in descending order the old code produced "aaa QUUX barQUUXo zzz".
func TestApplyTextEditsUnsortedInput(t *testing.T) {
	src := "aaa foo bar foo zzz\n"
	edits := []lsp.TextEdit{
		edit(0, 12, 0, 15, "QUUX"), // second occurrence, sent FIRST
		edit(0, 4, 0, 7, "QUUX"),   // first occurrence, sent SECOND
	}
	got, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16)
	if err != nil {
		t.Fatal(err)
	}
	if want := "aaa QUUX bar QUUX zzz\n"; string(got) != want {
		t.Fatalf("apply order depends on server ordering:\n got %q\nwant %q", got, want)
	}
}

func TestApplyTextEditsMultiLine(t *testing.T) {
	src := "package main\n\nfunc Add(a, b int) int { return a + b }\n\nvar _ = Add\n"
	edits := []lsp.TextEdit{
		edit(2, 5, 2, 8, "Sum"),
		edit(4, 8, 4, 11, "Sum"),
	}
	got, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16)
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\nfunc Sum(a, b int) int { return a + b }\n\nvar _ = Sum\n"
	if string(got) != want {
		t.Fatalf("multi-line rename wrong:\n got %q\nwant %q", got, want)
	}
}

func TestApplyTextEditsRejectsOverlap(t *testing.T) {
	src := "aaa bbb ccc\n"
	edits := []lsp.TextEdit{edit(0, 0, 0, 7, "X"), edit(0, 4, 0, 11, "Y")}
	if _, err := applyTextEdits([]byte(src), edits, lsp.PositionEncodingUTF16); err == nil {
		t.Fatal("overlapping edits must be refused, not silently mis-applied")
	}
}

// TestApplyWorkspaceEditIsAtomic pins that a failure part-way through leaves
// NOTHING written. The old version wrote each file as it iterated a Go map —
// random order — so a failure on the 4th of 7 files left a nondeterministic
// subset rewritten: a workspace that no longer compiles and cannot be
// reproduced.
func TestApplyWorkspaceEditIsAtomic(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "a.go")
	bad := filepath.Join(dir, "b.go")
	const goodSrc = "package a\n\nvar Foo = 1\n"
	const badSrc = "package b\n"
	if err := os.WriteFile(good, []byte(goodSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(badSrc), 0o644); err != nil {
		t.Fatal(err)
	}

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
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != goodSrc {
		t.Fatalf("a failed workspace edit left %s modified:\n%q", good, after)
	}
}

func TestApplyWorkspaceEditWritesEveryFile(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	byFile := map[string][]lsp.TextEdit{}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("package x\n\nvar Foo = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		byFile[p] = []lsp.TextEdit{edit(2, 4, 2, 7, "Bar")}
	}

	modified, err := applyWorkspaceEdit(byFile, lsp.PositionEncodingUTF16, tools.NewFileTracker())
	if err != nil {
		t.Fatal(err)
	}
	if len(modified) != len(paths) {
		t.Fatalf("modified %d files, want %d", len(modified), len(paths))
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "var Bar = 1") {
			t.Fatalf("%s was not edited: %q", p, b)
		}
	}
}
