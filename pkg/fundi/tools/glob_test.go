package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestGlobToolMatchesAndSortsByMtimeDescending(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	older := filepath.Join(dir, "older.go")
	newer := filepath.Join(dir, "newer.go")
	other := filepath.Join(dir, "ignored.txt")
	c.NoError(os.WriteFile(older, []byte("x"), 0o644))
	c.NoError(os.WriteFile(newer, []byte("x"), 0o644))
	c.NoError(os.WriteFile(other, []byte("x"), 0o644))
	base := time.Now()
	c.NoError(os.Chtimes(older, base, base))
	c.NoError(os.Chtimes(newer, base.Add(time.Hour), base.Add(time.Hour)))

	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	c.Len(lines, 2, "expected 2 matches, got")
	c.False(lines[0] != newer || lines[1] != older, "expected newer-first order, got %v", lines)
	c.NotStrContains(out, "ignored.txt", "pattern should not have matched ignored.txt")
}

func TestGlobToolNoMatches(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.nope","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "no", "expected a no-matches message, got")
}

func TestGlobToolCapsAt200(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	for i := 0; i < 250; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		c.NoError(os.WriteFile(p, []byte("x"), 0o644))
	}
	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 200 matched paths + one "[+N more]" trailer line.
	c.Len(lines, 201, "expected 201 output lines (200 matches + trailer), got %d", len(lines))
	c.StrContains(lines[len(lines)-1], "more", "expected a trailer mentioning more matches, got")
}

func TestGlobToolRecursivePattern(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	c.NoError(os.MkdirAll(nested, 0o755))
	p := filepath.Join(nested, "deep.go")
	c.NoError(os.WriteFile(p, []byte("x"), 0o644))
	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, p, "expected recursive match to include")
}

// TestGlobToolAbsolutePatternInsideBase guards the "confident wrong answer"
// bug: read/write/edit all require absolute paths, so the tool surface trains
// the model to pass one here too. An absolute pattern used to match nothing
// against the base-rooted fs and return a cheerful "no files matched".
func TestGlobToolAbsolutePatternInsideBase(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	c.NoError(os.MkdirAll(nested, 0o755))
	p := filepath.Join(nested, "a.go")
	c.NoError(os.WriteFile(p, []byte("x"), 0o644))

	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, filepath.Join(dir, "**", "*.go"), dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, p, "expected an absolute pattern inside path to be rebased and match")
}

// TestGlobToolAbsolutePatternOutsideBase: when the pattern can't be rebased,
// say so explicitly rather than reporting "no files matched".
func TestGlobToolAbsolutePatternOutsideBase(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	other := t.TempDir()

	tool := testGlobTool(t, "")
	_, err := tool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, filepath.Join(other, "*.go"), dir)))
	c.Error(err, "expected an explicit error for an absolute pattern outside path, got nil")
	c.StrContains(err.Error(), "relative to path", "expected the error to name the relative-to-path contract, got %v", err)
}

// TestGlobToolRespectsCanceledContext guards against a slow or huge glob
// walk continuing after the caller (agentloop's in-band abort) has given up
// on it.
func TestGlobToolRespectsCanceledContext(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := testGlobTool(t, "")
	_, err := tool.Execute(ctx, ToolInput(fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir)))
	c.Error(err, "expected an error for an already-canceled context")
	c.StrContains(err.Error(), "context canceled", "expected a context-canceled error, got %v", err)
}

// TestGlobToolExcludesGitignoredFiles verifies that .gitignore is honoured
// via DiscoverFiles' ripgrep backend (--no-require-git).
func TestGlobToolExcludesGitignoredFiles(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("x"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x"), 0o644))

	tool := testGlobTool(t, "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "kept.txt", "expected kept.txt to be included, got")
	c.NotStrContains(out, "ignored.txt", "expected ignored.txt to be excluded by .gitignore, got")
}

// TestGlobToolResolvesRelativePathAgainstCwd pins the fix for resolving a
// relative base against the agent's materialized working directory rather
// than the daemon's process cwd, which filepath.Abs used to do — and which
// is a different directory whenever fundi runs in-process in the daemon.
func TestGlobToolResolvesRelativePathAgainstCwd(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))

	tool := testGlobTool(t, dir)
	res, err := tool.Execute(context.Background(), ToolInput(`{"pattern":"*.go","path":"."}`))
	c.NoError(err)
	c.StrContains(res.Text, filepath.Join(dir, "a.go"), "relative path \".\" should resolve against cwd and find a.go, got")
}
