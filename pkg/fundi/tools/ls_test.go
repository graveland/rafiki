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

func testLsTool(t *testing.T, opts ToolOpts) *lsTool {
	t.Helper()
	lt, err := (&LsBlueprint{}).Materialize(opts)
	assert.NewAborting(t).NoError(err)
	return lt.(*lsTool)
}

func TestLsRendersTree(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
		c.NoError(os.WriteFile(p, []byte(content), 0o644))
	}
	mustWrite("a.go", "package a")
	mustWrite("b/b.go", "package b")
	mustWrite("b/c/c.go", "package c")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	// Tree rendering uses indentation.
	c.StrContains(out, "a.go", "expected a.go in tree output, got")
	c.StrContains(out, "b.go", "expected b.go in tree output, got")
	c.StrContains(out, "c.go", "expected c.go in tree output, got")
	// Tree structure: b.go and the c/ subtree should be indented under b/.
	c.StrContains(out, "b/", "expected b/ directory marker in tree output, got")
}

func TestLsDepthLimitsTraversal(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
		c.NoError(os.WriteFile(p, []byte("x"), 0o644))
	}
	mustWrite("a.go")
	mustWrite("sub/b.go")
	mustWrite("sub/deep/c.go")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"depth":1}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "a.go", "depth 1 should include root file a.go, got")
	c.StrContains(out, "sub/", "depth 1 should include the sub/ directory entry, got")
	c.False(strings.Contains(out, "b.go") || strings.Contains(out, "c.go"), "depth 1 should NOT include nested files b.go or c.go, got %q", out)
}

func TestLsIgnoreExcludesMatches(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
		c.NoError(os.WriteFile(p, []byte("x"), 0o644))
	}
	mustWrite("keep.go")
	mustWrite("skip.txt")
	mustWrite("also_skip.log")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"ignore":["*.txt","*.log"]}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "keep.go", "expected keep.go in output, got")
	c.False(strings.Contains(out, "skip.txt") || strings.Contains(out, "also_skip.log"), "ignore patterns should exclude .txt and .log files, got %q", out)
}

func TestLsHonoursGitignore(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644))
	c.NoError(os.MkdirAll(filepath.Join(dir, "ignored"), 0o755))
	c.NoError(os.WriteFile(filepath.Join(dir, "kept.go"), []byte("x"), 0o644))
	c.NoError(os.WriteFile(filepath.Join(dir, "ignored", "hidden.go"), []byte("x"), 0o644))

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "kept.go", "expected kept.go in output, got")
	c.False(strings.Contains(out, "hidden.go") || strings.Contains(out, "ignored"), ".gitignore'd directory should be excluded, got %q", out)
}

func TestLsTruncationReported(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	dir := t.TempDir()
	// Create more files than the default output budget can hold.
	for i := 0; i < 500; i++ {
		p := filepath.Join(dir, fmt.Sprintf("file_%04d.txt", i))
		c.NoError(os.WriteFile(p, []byte(strings.Repeat("x", 200)), 0o644))
	}

	lt := testLsTool(t, ToolOpts{
		Cwd:          dir,
		OutputPolicy: OutputPolicy{Budget: 500},
	})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	c.NoError(err)
	out := res.Text
	c.False(!strings.Contains(out, "elided") && !strings.Contains(out, "truncated"), "truncation should be reported in output, got %q", out)
}

func TestLsDefaultsToCwd(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "cwd-file.go")
	c.NoError(os.WriteFile(p, []byte("x"), 0o644))
	origWD, err := os.Getwd()
	c.NoError(err)
	c.NoError(os.Chdir(dir))
	defer func() {
		c.NoError(os.Chdir(origWD))
	}()

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err)
	out := res.Text
	c.StrContains(out, "cwd-file.go", "expected cwd-file.go when path defaults to cwd, got")
}

func TestLsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	assert.NewAborting(t).NoError(err)
	// Should not error on empty directory.
	_ = res.Text
}

// TestLsIgnoreSurvivesTruncation covers the lsMaxFiles branch, which had no
// coverage at all: the existing truncation test builds 500 files against a
// cap of 1000, so `truncated` is always false there.
//
// The bug this pins: the ignore filter used to reuse paths' backing array
// (paths[:0]), and the cap below then re-extended into it, resurrecting
// every entry the filter had just dropped. `ignore` silently did nothing on
// exactly the large directories it exists for, and because the re-extension
// stays within cap there was no panic to notice.
func TestLsIgnoreSurvivesTruncation(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	for i := 0; i < lsMaxFiles+100; i++ {
		c.NoError(os.WriteFile(filepath.Join(dir, fmt.Sprintf("noise-%04d.log", i)), []byte("x"), 0o644))
	}
	c.NoError(os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n"), 0o644))

	lt, err := (&LsBlueprint{}).Materialize(ToolOpts{Cwd: dir})
	c.NoError(err)
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"ignore":["*.log"]}`, dir)))
	c.NoError(err)
	c.NotStrContains(res.Text, ".log", "ignore was defeated by the file cap: .log entries present in output:\n%s", firstLines(res.Text, 10))
	c.StrContains(res.Text, "keep.go", "the one non-ignored file is missing from the listing:\n%s", firstLines(res.Text, 10))
}

// TestLsRelativePathUsesAgentCwd pins that a relative path resolves against
// the agent's cwd, not the daemon's process cwd. fundi runs in-process in
// the daemon, so those differ in production; filepath.Abs silently used the
// wrong one, and where both trees held a same-named directory the model got
// a listing of the wrong repo with no error.
func TestLsRelativePathUsesAgentCwd(t *testing.T) {
	c := assert.NewAborting(t)
	agentCwd := t.TempDir()
	sub := filepath.Join(agentCwd, "target")
	c.NoError(os.Mkdir(sub, 0o755))
	c.NoError(os.WriteFile(filepath.Join(sub, "marker.go"), []byte("package t\n"), 0o644))

	lt, err := (&LsBlueprint{}).Materialize(ToolOpts{Cwd: agentCwd})
	c.NoError(err)
	res, err := lt.Execute(context.Background(), ToolInput(`{"path":"target"}`))
	c.NoError(err, "relative path resolved against the wrong cwd")
	c.StrContains(res.Text, "marker.go", "expected the agent-cwd-relative listing, got:\n")
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
