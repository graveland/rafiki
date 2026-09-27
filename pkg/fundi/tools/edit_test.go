package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestEditToolNoMatch(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello world"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"nope","new_string":"x"}`, p)))
	c.False(err == nil || !strings.Contains(err.Error(), "not found"), "expected a not-found error, got %v", err)
}

func TestEditToolMultipleMatchesWithoutReplaceAll(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("foo foo foo"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"foo","new_string":"bar"}`, p)))
	c.False(err == nil || !strings.Contains(err.Error(), "3"), "expected an error mentioning the 3 matches, got %v", err)
	b, _ := os.ReadFile(p)
	c.Eq("foo foo foo", string(b), "file should be untouched, got %q", b)
}

func TestEditToolReplaceAll(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("foo foo foo"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"foo","new_string":"bar","replace_all":true}`, p)))
	c.NoError(err)
	b, _ := os.ReadFile(p)
	c.Eq("bar bar bar", string(b), "content = %q", b)
}

func TestEditToolStaleMtime(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello world"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}

	// Out-of-band modification after the read, with a deterministically
	// bumped mtime (no sleep-based flakiness).
	info, err := os.Stat(p)
	c.NoError(err)
	c.NoError(os.WriteFile(p, []byte("hello mars"), 0o644))
	newMtime := info.ModTime().Add(2 * time.Second)
	c.NoError(os.Chtimes(p, newMtime, newMtime))

	_, err = editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"bye"}`, p)))
	c.Error(err, "expected a staleness error")
	b, _ := os.ReadFile(p)
	c.Eq("hello mars", string(b), "file should be untouched by the rejected edit, got %q", b)
}

func TestEditToolChainedEditsNeedOnlyOneRead(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("one two three"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"one","new_string":"1"}`, p))); err != nil {
		t.Fatal(err)
	}
	// Second edit relies on the first edit's own RecordRead, not a fresh read.
	_, err := editTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"old_string":"two","new_string":"2"}`, p)))
	c.NoError(err, "expected chained edit to succeed, got")
	b, _ := os.ReadFile(p)
	c.Eq("1 2 three", string(b), "content = %q", b)
}

// TestEditToolConcurrentEditsAreNotLost is the regression test for the
// read-modify-write race: rafiki's agentloop runs a tool batch concurrently
// (errgroup, SetLimit(6)), and a model emitting several edits on one file in
// one batch is routine. Without per-path locking in FileTracker every
// goroutine verifies, reads the same pre-state, and the last write wins —
// silently discarding the others while reporting success to the model.
func TestEditToolConcurrentEditsAreNotLost(t *testing.T) {
	c := assert.NewCollecting(t)
	const n = 8

	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("t%d", i)
	}
	c.Require().NoError(os.WriteFile(p, []byte(strings.Join(tokens, " ")), 0o644))

	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = editTool.Execute(context.Background(), ToolInput(
				fmt.Sprintf(`{"path":%q,"old_string":"t%d","new_string":"X%d"}`, p, i, i)))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		c.Require().NoError(err, "edit %d failed", i)
	}

	b, err := os.ReadFile(p)
	c.Require().NoError(err)
	got := string(b)
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("X%d", i)
		c.StrContains(got, want, "edit %d was silently lost: %q missing from", i, want)
	}
}

func TestEditToolRelativePathRejected(t *testing.T) {
	tr := NewFileTracker()
	editTool := testEditTool(t, tr, "")
	_, err := editTool.Execute(context.Background(), ToolInput(`{"path":"rel.txt","old_string":"a","new_string":"b"}`))
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "absolute"), "expected an absolute-path error, got %v", err)
}

func TestEditToolCRLFFile(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello\r\nworld\r\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// old_string uses LF — edit normalizes both file content and old_string to LF.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"old_string":"hello\nworld","new_string":"hi\nthere"}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	// The file should keep its original CRLF line endings.
	c.Eq("hi\r\nthere\r\n", got, "expected 'hi\\r\\nthere\\r\\n', got")
}

func TestEditToolBOMHandling(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("\uFEFFhello\nworld\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// old_string does NOT include the BOM (the model won't emit invisible BOM).
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"old_string":"hello\n","new_string":"hi\n"}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	c.Eq("\uFEFFhi\nworld\n", got, "expected BOM-preserved content, got")
}

func TestEditToolFuzzySmartQuotes(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	// File has straight quotes.
	c.NoError(os.WriteFile(p, []byte(`var msg = "hello world"`+"\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Model emits curly quotes — fuzzy matching normalizes them.
	curly := ToolInput(fmt.Sprintf(
		`{"path":%q,"old_string":%q,"new_string":%q}`, p,
		"var msg = \u201Chello world\u201D",
		`var msg = "hi there"`,
	))
	_, err := editTool.Execute(context.Background(), curly)
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	c.Eq("var msg = \"hi there\"\n", got, "fuzzy smart-quote edit failed, got")
}

func TestEditToolFuzzyTrailingWhitespace(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	// File has no trailing whitespace.
	c.NoError(os.WriteFile(p, []byte("func foo() {\n    bar()\n}\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Model emits old_string with trailing spaces (common LLM artifact).
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"old_string":"    bar()  ","new_string":"    baz()"}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	c.Eq("func foo() {\n    baz()\n}\n", got, "trailing-whitespace fuzzy edit failed, got")
}

func TestEditToolFuzzyUnicodeDash(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	// File uses ASCII hyphen.
	c.NoError(os.WriteFile(p, []byte("long-term\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Model emits en-dash.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"old_string":"long\u2013term","new_string":"short-term"}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	c.Eq("short-term\n", got, "unicode-dash fuzzy edit failed, got")
}

func TestEditToolMultiEditExact(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("a=1\nb=2\nc=3\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"edits":[
			{"old_string":"a=1","new_string":"a=10"},
			{"old_string":"c=3","new_string":"c=30"}
		]}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	c.Eq("a=10\nb=2\nc=30\n", got, "multi-edit failed, got")
}

func TestEditToolMultiEditOverlapRejected(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello world\n"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"edits":[
			{"old_string":"hello world","new_string":"hi there"},
			{"old_string":"world","new_string":"planet"}
		]}`, p),
	))
	c.False(err == nil || !strings.Contains(err.Error(), "overlap"), "expected overlap error, got %v", err)
}

func TestEditToolFuzzyPreservesUnchangedLines(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	// File uses tabs for indentation on unchanged lines.
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")  \n}\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Fuzzy match needed on the line we're editing (has trailing spaces on
	// one line, which triggers fuzzy matching for the whole edit).
	// The untouched lines should keep their original tabs.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"old_string":"\tfmt.Println(\"world\")  ","new_string":"\tfmt.Println(\"universe\")"}`, p),
	))
	c.NoError(err, "unexpected error")
	b, _ := os.ReadFile(p)
	got := string(b)
	want := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"universe\")\n}\n"
	c.Eq(want, got, "unchanged-line preservation failed: got")
}

func TestEditToolFilePathAlias(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello world"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, dir)
	editTool := testEditTool(t, tr, dir)
	// Read via file_path alias.
	if _, err := readTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"file_path":%q}`, p),
	)); err != nil {
		t.Fatalf("read via file_path failed: %v", err)
	}
	// Edit via file_path alias.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"file_path":%q,"old_string":"hello","new_string":"hi"}`, p),
	))
	c.NoError(err, "edit via file_path failed")
	b, _ := os.ReadFile(p)
	c.Eq("hi world", string(b), "content = %q", b)
}

func TestEditToolRelativePath(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "a.txt")
	c.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
	c.NoError(os.WriteFile(p, []byte("hello relative"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, dir)
	editTool := testEditTool(t, tr, dir)
	// Read with a relative path resolved against cwd.
	if _, err := readTool.Execute(context.Background(), ToolInput(`{"path":"sub/a.txt"}`)); err != nil {
		t.Fatalf("read relative failed: %v", err)
	}
	// Edit with a relative path.
	_, err := editTool.Execute(context.Background(), ToolInput(
		`{"path":"sub/a.txt","old_string":"hello relative","new_string":"bye relative"}`,
	))
	c.NoError(err, "edit relative failed")
	b, _ := os.ReadFile(p)
	c.Eq("bye relative", string(b), "content = %q", b)
}

func TestEditToolTildeExpansion(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("tilde test"), 0o644))
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, dir)
	_, err := readTool.Execute(context.Background(), ToolInput(`{"path":"~/a.txt"}`))
	c.NoError(err, "read via ~ failed")
}
