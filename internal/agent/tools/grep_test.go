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

func TestGrepToolDefaultsToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
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

	fn := newGrepTool()
	out, err := fn(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "needle") {
		t.Fatalf("expected default path to be the working directory, got %q", out)
	}
}
