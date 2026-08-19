package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepToolFindsMatchesInPathLineTextFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\nfunc Foo() {}\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	want := fmt.Sprintf("%s:2:func Foo() {}\n", p)
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestGrepToolExcludesGitDir(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if strings.Contains(out, ".git") {
		t.Fatalf("expected .git to be excluded, got %q", out)
	}
	if !strings.Contains(out, "real.txt") {
		t.Fatalf("expected real.txt match, got %q", out)
	}
}

func TestGrepToolGlobFilter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if strings.Contains(out, "b.txt") {
		t.Fatalf("expected b.txt to be filtered out, got %q", out)
	}
	if !strings.Contains(out, "a.go") {
		t.Fatalf("expected a.go match, got %q", out)
	}
}

// TestGrepToolGlobFilterMatchesNestedFiles guards the silent-under-reporting
// bug: doublestar's `*` does not cross a path separator, so matching a bare
// "*.go" against the base-relative path searched only top-level files and
// reported no/partial matches with no indication anything was skipped. The
// model's prior is ripgrep's -g '*.go', which matches at any depth.
func TestGrepToolGlobFilterMatchesNestedFiles(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(dir, "a.go")
	deep := filepath.Join(nested, "b.go")
	skipped := filepath.Join(nested, "c.txt")
	for _, p := range []string{top, deep, skipped} {
		if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, top) {
		t.Errorf("expected the top-level %q in %q", top, out)
	}
	if !strings.Contains(out, deep) {
		t.Errorf("expected the nested %q in %q — a separator-free glob must match at any depth", deep, out)
	}
	if strings.Contains(out, skipped) {
		t.Errorf("expected %q to be filtered out, got %q", skipped, out)
	}
}

// TestGrepToolGlobWithSeparatorStaysPathRelative checks the basename fallback
// didn't loosen patterns that do carry a separator — those stay anchored to
// the base-relative path.
func TestGrepToolGlobWithSeparatorStaysPathRelative(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inSub := filepath.Join(sub, "b.go")
	atTop := filepath.Join(dir, "a.go")
	for _, p := range []string{inSub, atTop} {
		if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"sub/*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, inSub) {
		t.Errorf("expected %q in %q", inSub, out)
	}
	if strings.Contains(out, atTop) {
		t.Errorf("expected %q to be excluded by a path-anchored glob, got %q", atTop, out)
	}
}

func TestGrepToolMaxMatchesTrailer(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "needle")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"max_matches":3}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	got := strings.Count(out, "needle")
	// 3 shown occurrences on their own lines + the trailer does not itself
	// contain the literal word "needle".
	if got != 3 {
		t.Fatalf("expected 3 shown matches, got %d in %q", got, out)
	}
	if !strings.Contains(out, "more") {
		t.Fatalf("expected a trailer mentioning more matches, got %q", out)
	}
}

func TestGrepToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"zzz","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "no") {
		t.Fatalf("expected a no-matches message, got %q", out)
	}
}

func TestGrepToolInvalidPattern(t *testing.T) {
	dir := t.TempDir()
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"(","path":%q}`, dir)))
	if err == nil {
		t.Fatal("expected a regexp compile error")
	}
}

// TestGrepToolRespectsCanceledContext guards against a slow or huge tree
// walk continuing after the caller (agentloop's in-band abort) has given up
// on it.
func TestGrepToolRespectsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := testGrepTool(t, "")
	_, err := tool.Execute(ctx, ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected a context-canceled error, got %v", err)
	}
}

func TestGrepToolRequiresPath(t *testing.T) {
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle"}`))
	if err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

// TestGrepToolRejectsFilesystemRoot guards against a model (accidentally or
// otherwise) walking the entire disk via path:"/".
func TestGrepToolRejectsFilesystemRoot(t *testing.T) {
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle","path":"/"}`))
	if err == nil {
		t.Fatal("expected an error for path \"/\"")
	}
}

// TestGrepToolEmitsAbsolutePathsForRelativeBase: grep's output is the model's
// input to read and edit, and both of those *reject* a relative path. A
// relative base therefore produced "path:line:text" lines the model could not
// feed back into any other tool. glob already absolutizes; grep must too.
func TestGrepToolEmitsAbsolutePathsForRelativeBase(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "a.go")
	if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// cwd = dir, not t.Chdir: the agent's working directory is captured at
	// materialization, and grep must resolve the relative base against it —
	// not against the daemon's process cwd, which t.Chdir would only pretend
	// is the same thing.
	tool := testGrepTool(t, dir)
	res, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle","path":"sub"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text

	line := strings.TrimRight(out, "\n")
	emitted, _, ok := strings.Cut(line, ":")
	if !ok {
		t.Fatalf("expected a path:line:text match, got %q", out)
	}
	if !filepath.IsAbs(emitted) {
		t.Fatalf("grep emitted the relative path %q; read/edit reject relative paths, so the model cannot reuse it", emitted)
	}

	// The real contract: the emitted path must be directly usable by read.
	tr := NewFileTracker()
	if _, err := testReadTool(t, tr, "").Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, emitted))); err != nil {
		t.Fatalf("read rejected grep's own output %q: %v", emitted, err)
	}
}

func TestGrepToolSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	want := fmt.Sprintf("%s:2:func Foo() {}\n", p)
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

// TestGrepToolExcludesGitignoredFiles verifies that .gitignore is honoured
// via SearchContent's ripgrep backend.
func TestGrepToolExcludesGitignoredFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "kept.txt") {
		t.Fatalf("expected kept.txt to be included, got %q", out)
	}
	if strings.Contains(out, "ignored.txt") {
		t.Fatalf("expected ignored.txt to be excluded by .gitignore, got %q", out)
	}
}

// TestGrepToolNoMatchesErrorMessage verifies that zero matches returns
// the "no matches" message and not an error.
func TestGrepToolNoMatchesErrorMessage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err != nil {
		t.Fatalf("zero matches must not be an error, got %v", err)
	}
	if res.Text != "no matches" {
		t.Fatalf("expected 'no matches', got %q", res.Text)
	}
}

// TestGrepToolMaxMatchesHonouredWithGlob verifies max_matches works
// together with a glob filter.
func TestGrepToolMaxMatchesHonouredWithGlob(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\nneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"max_matches":1,"glob":"*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	got := strings.Count(out, "needle")
	if got != 1 {
		t.Fatalf("expected 1 shown match, got %d in %q", got, out)
	}
}

// TestGrepSingleFileWithGlob: when path names one file, filepath.Rel yields
// "." and no glob can match it, so every hit was discarded and grep answered
// "no matches" for a file that plainly contained the pattern. Naming a file
// is already narrower than any glob, so the filter must not apply.
func TestGrepSingleFileWithGlob(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q,"glob":"*.go"}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "no matches") {
		t.Fatalf("glob defeated a single-file search:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "func Foo") {
		t.Fatalf("expected the match, got:\n%s", res.Text)
	}
}

// TestGrepDashPattern: without -e and --, ripgrep parses a pattern starting
// with a dash as a flag and the search dies with "unrecognized flag".
func TestGrepDashPattern(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("run --force now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"--force","path":%q}`, dir)))
	if err != nil {
		t.Fatalf("a pattern starting with a dash must not error: %v", err)
	}
	if !strings.Contains(res.Text, "--force") {
		t.Fatalf("expected the match, got:\n%s", res.Text)
	}
}

// TestGrepFindsHiddenFiles: rg skips dotfiles by default, so .env.example
// and .github/ were invisible and grep answered "no matches" for content
// that exists.
func TestGrepFindsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("RAFIKI_TOKEN=xyz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"RAFIKI_TOKEN","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, ".env.example") {
		t.Fatalf("hidden files must be searched, got:\n%s", res.Text)
	}
}
