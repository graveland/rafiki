package tools

import (
	"context"
	"encoding/json"
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

	fn := newGlobTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	fn := newGlobTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"*.nope","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
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
	fn := newGlobTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 200 matched paths + one "[+N more]" trailer line.
	if len(lines) != 201 {
		t.Fatalf("expected 201 output lines (200 matches + trailer), got %d", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "+50") {
		t.Fatalf("expected a trailer mentioning 50 more, got %q", lines[len(lines)-1])
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
	fn := newGlobTool()
	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, dir)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, p) {
		t.Fatalf("expected recursive match to include %q, got %q", p, out)
	}
}

func TestGlobToolDefaultsToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cwd-match.go")
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

	fn := newGlobTool()
	out, err := fn(context.Background(), json.RawMessage(`{"pattern":"*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cwd-match.go") {
		t.Fatalf("expected default path to be the working directory, got %q", out)
	}
}
