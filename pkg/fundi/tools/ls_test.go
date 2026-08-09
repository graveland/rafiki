package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLsTool(t *testing.T, opts ToolOpts) *lsTool {
	t.Helper()
	lt, err := (&LsBlueprint{}).Materialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	return lt.(*lsTool)
}

func TestLsRendersTree(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.go", "package a")
	mustWrite("b/b.go", "package b")
	mustWrite("b/c/c.go", "package c")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	// Tree rendering uses indentation.
	if !strings.Contains(out, "a.go") {
		t.Fatalf("expected a.go in tree output, got %q", out)
	}
	if !strings.Contains(out, "b.go") {
		t.Fatalf("expected b.go in tree output, got %q", out)
	}
	if !strings.Contains(out, "c.go") {
		t.Fatalf("expected c.go in tree output, got %q", out)
	}
	// Tree structure: b.go and the c/ subtree should be indented under b/.
	if !strings.Contains(out, "b/") {
		t.Fatalf("expected b/ directory marker in tree output, got %q", out)
	}
}

func TestLsDepthLimitsTraversal(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.go")
	mustWrite("sub/b.go")
	mustWrite("sub/deep/c.go")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"depth":1}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "a.go") {
		t.Fatalf("depth 1 should include root file a.go, got %q", out)
	}
	if !strings.Contains(out, "sub/") {
		t.Fatalf("depth 1 should include the sub/ directory entry, got %q", out)
	}
	if strings.Contains(out, "b.go") || strings.Contains(out, "c.go") {
		t.Fatalf("depth 1 should NOT include nested files b.go or c.go, got %q", out)
	}
}

func TestLsIgnoreExcludesMatches(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("keep.go")
	mustWrite("skip.txt")
	mustWrite("also_skip.log")

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"ignore":["*.txt","*.log"]}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "keep.go") {
		t.Fatalf("expected keep.go in output, got %q", out)
	}
	if strings.Contains(out, "skip.txt") || strings.Contains(out, "also_skip.log") {
		t.Fatalf("ignore patterns should exclude .txt and .log files, got %q", out)
	}
}

func TestLsHonoursGitignore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored", "hidden.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "kept.go") {
		t.Fatalf("expected kept.go in output, got %q", out)
	}
	if strings.Contains(out, "hidden.go") || strings.Contains(out, "ignored") {
		t.Fatalf(".gitignore'd directory should be excluded, got %q", out)
	}
}

func TestLsTruncationReported(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	dir := t.TempDir()
	// Create more files than the default output budget can hold.
	for i := 0; i < 500; i++ {
		p := filepath.Join(dir, fmt.Sprintf("file_%04d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 200)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	lt := testLsTool(t, ToolOpts{
		Cwd:          dir,
		OutputPolicy: OutputPolicy{Budget: 500},
	})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "elided") && !strings.Contains(out, "truncated") {
		t.Fatalf("truncation should be reported in output, got %q", out)
	}
}

func TestLsDefaultsToCwd(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cwd-file.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatal(err)
		}
	}()

	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "cwd-file.go") {
		t.Fatalf("expected cwd-file.go when path defaults to cwd, got %q", out)
	}
}

func TestLsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	lt := testLsTool(t, ToolOpts{Cwd: dir})
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	dir := t.TempDir()
	for i := 0; i < lsMaxFiles+100; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("noise-%04d.log", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lt, err := (&LsBlueprint{}).Materialize(ToolOpts{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	res, err := lt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q,"ignore":["*.log"]}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, ".log") {
		t.Fatalf("ignore was defeated by the file cap: .log entries present in output:\n%s", firstLines(res.Text, 10))
	}
	if !strings.Contains(res.Text, "keep.go") {
		t.Fatalf("the one non-ignored file is missing from the listing:\n%s", firstLines(res.Text, 10))
	}
}

// TestLsRelativePathUsesAgentCwd pins that a relative path resolves against
// the agent's cwd, not the daemon's process cwd. fundi runs in-process in
// the daemon, so those differ in production; filepath.Abs silently used the
// wrong one, and where both trees held a same-named directory the model got
// a listing of the wrong repo with no error.
func TestLsRelativePathUsesAgentCwd(t *testing.T) {
	agentCwd := t.TempDir()
	sub := filepath.Join(agentCwd, "target")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "marker.go"), []byte("package t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lt, err := (&LsBlueprint{}).Materialize(ToolOpts{Cwd: agentCwd})
	if err != nil {
		t.Fatal(err)
	}
	res, err := lt.Execute(context.Background(), ToolInput(`{"path":"target"}`))
	if err != nil {
		t.Fatalf("relative path resolved against the wrong cwd: %v", err)
	}
	if !strings.Contains(res.Text, "marker.go") {
		t.Fatalf("expected the agent-cwd-relative listing, got:\n%s", res.Text)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
