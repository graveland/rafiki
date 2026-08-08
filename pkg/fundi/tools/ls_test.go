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
