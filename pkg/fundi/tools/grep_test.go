package tools

import (
	"context"
	"encoding/json"
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
	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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

	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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

	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"sub/*.go"}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q,"max_matches":3}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Count(out, "needle")
	// 3 shown occurrences on their own lines + the trailer does not itself
	// contain the literal word "needle".
	if got != 3 {
		t.Fatalf("expected 3 shown matches, got %d in %q", got, out)
	}
	if !strings.Contains(out, "+7") {
		t.Fatalf("expected a trailer mentioning 7 more, got %q", out)
	}
}

func TestGrepToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"zzz","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no") {
		t.Fatalf("expected a no-matches message, got %q", out)
	}
}

func TestGrepToolInvalidPattern(t *testing.T) {
	dir := t.TempDir()
	fn := newGrepTool()
	_, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"(","path":%q}`, dir)))
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

	fn := newGrepTool()
	_, err := fn(ctx, json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected a context-canceled error, got %v", err)
	}
}

func TestGrepToolRequiresPath(t *testing.T) {
	fn := newGrepTool()
	_, err := fn(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

// TestGrepToolRejectsFilesystemRoot guards against a model (accidentally or
// otherwise) walking the entire disk via path:"/".
func TestGrepToolRejectsFilesystemRoot(t *testing.T) {
	fn := newGrepTool()
	_, err := fn(context.Background(), json.RawMessage(`{"pattern":"needle","path":"/"}`))
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
	t.Chdir(dir)

	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(`{"pattern":"needle","path":"sub"}`))
	if err != nil {
		t.Fatal(err)
	}

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
	if _, err := newReadTool(tr)(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, emitted))); err != nil {
		t.Fatalf("read rejected grep's own output %q: %v", emitted, err)
	}
}

func TestGrepToolSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s:2:func Foo() {}\n", p)
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}
