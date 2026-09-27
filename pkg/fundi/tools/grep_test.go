package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestGrepToolFindsMatchesInPathLineTextFormat(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	c.NoError(os.WriteFile(p, []byte("package a\nfunc Foo() {}\nfunc Bar() {}\n"), 0o644))
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	want := fmt.Sprintf("%s:2:func Foo() {}\n", p)
	c.Eq(want, out, "got")
}

func TestGrepToolExcludesGitDir(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	c.NoError(os.MkdirAll(gitDir, 0o755))
	c.NoError(os.WriteFile(filepath.Join(gitDir, "config"), []byte("needle\n"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "real.txt"), []byte("needle\n"), 0o644))
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.NotStrContains(out, ".git", "expected .git to be excluded, got")
	c.StrContains(out, "real.txt", "expected real.txt match, got")
}

func TestGrepToolGlobFilter(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "b.txt"), []byte("needle\n"), 0o644))
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	c.NoError(err)
	out := res.Text
	c.NotStrContains(out, "b.txt", "expected b.txt to be filtered out, got")
	c.StrContains(out, "a.go", "expected a.go match, got")
}

// TestGrepToolGlobFilterMatchesNestedFiles guards the silent-under-reporting
// bug: doublestar's `*` does not cross a path separator, so matching a bare
// "*.go" against the base-relative path searched only top-level files and
// reported no/partial matches with no indication anything was skipped. The
// model's prior is ripgrep's -g '*.go', which matches at any depth.
func TestGrepToolGlobFilterMatchesNestedFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "deeper")
	c.Require().NoError(os.MkdirAll(nested, 0o755))
	top := filepath.Join(dir, "a.go")
	deep := filepath.Join(nested, "b.go")
	skipped := filepath.Join(nested, "c.txt")
	for _, p := range []string{top, deep, skipped} {
		c.Require().NoError(os.WriteFile(p, []byte("needle\n"), 0o644))
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"*.go"}`, dir)))
	c.Require().NoError(err)
	out := res.Text
	c.StrContains(out, top, "expected the top-level")
	c.StrContains(out, deep, "expected the nested")
	c.NotStrContains(out, skipped, "expected")
}

// TestGrepToolGlobWithSeparatorStaysPathRelative checks the basename fallback
// didn't loosen patterns that do carry a separator — those stay anchored to
// the base-relative path.
func TestGrepToolGlobWithSeparatorStaysPathRelative(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	c.Require().NoError(os.MkdirAll(sub, 0o755))
	inSub := filepath.Join(sub, "b.go")
	atTop := filepath.Join(dir, "a.go")
	for _, p := range []string{inSub, atTop} {
		c.Require().NoError(os.WriteFile(p, []byte("needle\n"), 0o644))
	}

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"glob":"sub/*.go"}`, dir)))
	c.Require().NoError(err)
	out := res.Text
	c.StrContains(out, inSub, "expected")
	c.NotStrContains(out, atTop, "expected")
}

func TestGrepToolMaxMatchesTrailer(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "needle")
	}
	c.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"max_matches":3}`, dir)))
	c.NoError(err)
	out := res.Text
	got := strings.Count(out, "needle")
	// 3 shown occurrences on their own lines + the trailer does not itself
	// contain the literal word "needle".
	c.Eq(3, got, "expected 3 shown matches, got %d in %q", got, out)
	c.StrContains(out, "more", "expected a trailer mentioning more matches, got")
}

func TestGrepToolNoMatches(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))
	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"zzz","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "no", "expected a no-matches message, got")
}

func TestGrepToolInvalidPattern(t *testing.T) {
	dir := t.TempDir()
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"(","path":%q}`, dir)))
	assert.NewAborting(t).Error(err, "expected a regexp compile error")
}

// TestGrepToolRespectsCanceledContext guards against a slow or huge tree
// walk continuing after the caller (agentloop's in-band abort) has given up
// on it.
func TestGrepToolRespectsCanceledContext(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := testGrepTool(t, "")
	_, err := tool.Execute(ctx, ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	c.Error(err, "expected an error for an already-canceled context")
	c.StrContains(err.Error(), "context canceled", "expected a context-canceled error, got %v", err)
}

func TestGrepToolRequiresPath(t *testing.T) {
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle"}`))
	assert.NewAborting(t).Error(err, "expected an error for a missing path")
}

// TestGrepToolRejectsFilesystemRoot guards against a model (accidentally or
// otherwise) walking the entire disk via path:"/".
func TestGrepToolRejectsFilesystemRoot(t *testing.T) {
	tool := testGrepTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle","path":"/"}`))
	assert.NewAborting(t).Error(err, "expected an error for path \"/\"")
}

// TestGrepToolEmitsAbsolutePathsForRelativeBase: grep's output is the model's
// input to read and edit, and both of those *reject* a relative path. A
// relative base therefore produced "path:line:text" lines the model could not
// feed back into any other tool. glob already absolutizes; grep must too.
func TestGrepToolEmitsAbsolutePathsForRelativeBase(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	c.NoError(os.MkdirAll(sub, 0o755))
	p := filepath.Join(sub, "a.go")
	c.NoError(os.WriteFile(p, []byte("needle\n"), 0o644))

	// cwd = dir, not t.Chdir: the agent's working directory is captured at
	// materialization, and grep must resolve the relative base against it —
	// not against the daemon's process cwd, which t.Chdir would only pretend
	// is the same thing.
	tool := testGrepTool(t, dir)
	res, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"needle","path":"sub"}`))
	c.NoError(err)
	out := res.Text

	line := strings.TrimRight(out, "\n")
	emitted, _, ok := strings.Cut(line, ":")
	c.True(ok, "expected a path:line:text match, got %q", out)
	c.True(filepath.IsAbs(emitted), "grep emitted the relative path %q; read/edit reject relative paths, so the model cannot reuse it", emitted)

	// The real contract: the emitted path must be directly usable by read.
	tr := NewFileTracker()
	if _, err := testReadTool(t, tr, "").Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, emitted))); err != nil {
		t.Fatalf("read rejected grep's own output %q: %v", emitted, err)
	}
}

func TestGrepToolSingleFile(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	c.NoError(os.WriteFile(p, []byte("package a\nfunc Foo() {}\n"), 0o644))

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q}`, p)))
	c.NoError(err)
	out := res.Text
	want := fmt.Sprintf("%s:2:func Foo() {}\n", p)
	c.Eq(want, out, "got")
}

// TestGrepToolExcludesGitignoredFiles verifies that .gitignore is honoured
// via SearchContent's ripgrep backend.
func TestGrepToolExcludesGitignoredFiles(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("needle\n"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("needle\n"), 0o644))

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "kept.txt", "expected kept.txt to be included, got")
	c.NotStrContains(out, "ignored.txt", "expected ignored.txt to be excluded by .gitignore, got")
}

// TestGrepToolNoMatchesErrorMessage verifies that zero matches returns
// the "no matches" message and not an error.
func TestGrepToolNoMatchesErrorMessage(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("nothing here\n"), 0o644))

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	c.NoError(err, "zero matches must not be an error, got")
	c.Eq("no matches", res.Text, "expected 'no matches', got")
}

// TestGrepToolMaxMatchesHonouredWithGlob verifies max_matches works
// together with a glob filter.
func TestGrepToolMaxMatchesHonouredWithGlob(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\nneedle\n"), 0o644))

	tool := testGrepTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"needle","path":%q,"max_matches":1,"glob":"*.go"}`, dir)))
	c.NoError(err)
	out := res.Text
	got := strings.Count(out, "needle")
	c.Eq(1, got, "expected 1 shown match, got %d in %q", got, out)
}

// TestGrepSingleFileWithGlob: when path names one file, filepath.Rel yields
// "." and no glob can match it, so every hit was discarded and grep answered
// "no matches" for a file that plainly contained the pattern. Naming a file
// is already narrower than any glob, so the filter must not apply.
func TestGrepSingleFileWithGlob(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	c.NoError(os.WriteFile(p, []byte("package a\n\nfunc Foo() {}\n"), 0o644))

	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"func Foo","path":%q,"glob":"*.go"}`, p)))
	c.NoError(err)
	c.NotStrContains(res.Text, "no matches", "glob defeated a single-file search:\n")
	c.StrContains(res.Text, "func Foo", "expected the match, got:\n")
}

// TestGrepDashPattern: without -e and --, ripgrep parses a pattern starting
// with a dash as a flag and the search dies with "unrecognized flag".
func TestGrepDashPattern(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "x.txt"), []byte("run --force now\n"), 0o644))
	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"--force","path":%q}`, dir)))
	c.NoError(err, "a pattern starting with a dash must not error")
	c.StrContains(res.Text, "--force", "expected the match, got:\n")
}

// TestGrepFindsHiddenFiles: rg skips dotfiles by default, so .env.example
// and .github/ were invisible and grep answered "no matches" for content
// that exists.
func TestGrepFindsHiddenFiles(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, ".env.example"), []byte("RAFIKI_TOKEN=xyz\n"), 0o644))
	gt := testGrepTool(t, "")
	res, err := gt.Execute(context.Background(),
		ToolInput(fmt.Sprintf(`{"pattern":"RAFIKI_TOKEN","path":%q}`, dir)))
	c.NoError(err)
	c.StrContains(res.Text, ".env.example", "hidden files must be searched, got:\n")
}
