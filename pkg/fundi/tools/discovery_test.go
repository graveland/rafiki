package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// discoveryFixture builds a tree with a gitignored directory. The
// gitignore exclusion is the entire reason this package exists.
func discoveryFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "ignored/\n")
	write("keep.go", "package main\nconst Needle = 1\n")
	write("nested/also.go", "package nested\nconst Needle = 2\n")
	write("ignored/hidden.go", "package ignored\nconst Needle = 3\n")
	return root
}

func TestDiscoverFilesRespectsGitignore(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	paths, _, err := DiscoverFiles(context.Background(), FileQuery{Root: root})
	if err != nil {
		t.Fatalf("DiscoverFiles: %v", err)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "keep.go") || !strings.Contains(joined, "also.go") {
		t.Fatalf("expected tracked files, got %v", paths)
	}
	if strings.Contains(joined, "hidden.go") {
		t.Fatalf("gitignored file was returned: %v", paths)
	}
}

func TestDiscoverFilesGlobAndLimit(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	paths, truncated, err := DiscoverFiles(context.Background(), FileQuery{Root: root, Glob: "*.go", Limit: 1})
	if err != nil {
		t.Fatalf("DiscoverFiles: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("Limit not honoured: got %d paths", len(paths))
	}
	if !truncated {
		t.Fatal("truncated should be true when Limit cut the result")
	}
}

func TestSearchContentRespectsGitignore(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	matches, _, err := SearchContent(context.Background(), ContentQuery{Root: root, Pattern: "Needle"})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2 (the gitignored one must be excluded): %+v", len(matches), matches)
	}
	for _, m := range matches {
		if m.Line == 0 || m.Text == "" || m.Path == "" {
			t.Fatalf("incomplete match: %+v", m)
		}
	}
}

// TestSearchContentLimitTruncatesWithoutHangOrError exercises the early
// stdout-close path: with many matches available and a small Limit,
// SearchContent must stop reading, report truncated, and return promptly
// with no error even though it stopped rg mid-stream (via a closed pipe,
// which on this platform surfaces as EPIPE/SIGPIPE to the child).
func TestSearchContentLimitTruncatesWithoutHangOrError(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("const Needle = 1\n")
	}
	if err := os.WriteFile(filepath.Join(root, "big.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var matches []Match
	var truncated bool
	var err error
	go func() {
		matches, truncated, err = SearchContent(context.Background(), ContentQuery{Root: root, Pattern: "Needle", Limit: 3})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SearchContent hung after closing stdout early")
	}
	if err != nil {
		t.Fatalf("SearchContent returned a spurious error on early close: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3", len(matches))
	}
	if !truncated {
		t.Fatal("truncated should be true when Limit cut the result")
	}
}

func TestRgErrorTreatsExitOneAsNoMatches(t *testing.T) {
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.go"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matches, truncated, err := SearchContent(context.Background(), ContentQuery{Root: root, Pattern: "NoSuchNeedle"})
	if err != nil {
		t.Fatalf("SearchContent: unexpected error for a clean no-match search: %v", err)
	}
	if truncated {
		t.Fatal("truncated should be false when nothing matched")
	}
	if len(matches) != 0 {
		t.Fatalf("got %d matches, want 0", len(matches))
	}
}
