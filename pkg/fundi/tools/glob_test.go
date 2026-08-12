package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGlobToolMatchesAndSortsByMtimeDescending(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.go")
	newer := filepath.Join(dir, "newer.go")
	other := filepath.Join(dir, "ignored.txt")
	if err := os.WriteFile(older, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	if err := os.Chtimes(older, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 matches, got %v", lines)
	}
	if lines[0] != newer || lines[1] != older {
		t.Fatalf("expected newer-first order, got %v", lines)
	}
	if strings.Contains(out, "ignored.txt") {
		t.Fatalf("pattern should not have matched ignored.txt: %q", out)
	}
}

func TestGlobToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.nope","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "no") {
		t.Fatalf("expected a no-matches message, got %q", out)
	}
}

func TestGlobToolCapsAt200(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 250; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 200 matched paths + one "[+N more]" trailer line.
	if len(lines) != 201 {
		t.Fatalf("expected 201 output lines (200 matches + trailer), got %d", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "more") {
		t.Fatalf("expected a trailer mentioning more matches, got %q", lines[len(lines)-1])
	}
}

func TestGlobToolRecursivePattern(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(nested, "deep.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, p) {
		t.Fatalf("expected recursive match to include %q, got %q", p, out)
	}
}

// TestGlobToolAbsolutePatternInsideBase guards the "confident wrong answer"
// bug: read/write/edit all require absolute paths, so the tool surface trains
// the model to pass one here too. An absolute pattern used to match nothing
// against the base-rooted fs and return a cheerful "no files matched".
func TestGlobToolAbsolutePatternInsideBase(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(nested, "a.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, filepath.Join(dir, "**", "*.go"), dir)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, p) {
		t.Fatalf("expected an absolute pattern inside path to be rebased and match %q, got %q", p, out)
	}
}

// TestGlobToolAbsolutePatternOutsideBase: when the pattern can't be rebased,
// say so explicitly rather than reporting "no files matched".
func TestGlobToolAbsolutePatternOutsideBase(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()

	tool := &GlbTool{}
	_, err := tool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, filepath.Join(other, "*.go"), dir)))
	if err == nil {
		t.Fatal("expected an explicit error for an absolute pattern outside path, got nil")
	}
	if !strings.Contains(err.Error(), "relative to path") {
		t.Fatalf("expected the error to name the relative-to-path contract, got %v", err)
	}
}

// TestGlobToolRespectsCanceledContext guards against a slow or huge glob
// walk continuing after the caller (agentloop's in-band abort) has given up
// on it.
func TestGlobToolRespectsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := &GlbTool{}
	_, err := tool.Execute(ctx, ToolInput(fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir)))
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected a context-canceled error, got %v", err)
	}
}

// TestGlobToolExcludesGitignoredFiles verifies that .gitignore is honoured
// via DiscoverFiles' ripgrep backend (--no-require-git).
func TestGlobToolExcludesGitignoredFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GlbTool{}
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, dir)))
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
