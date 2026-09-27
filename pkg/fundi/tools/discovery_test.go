package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// discoveryFixture builds a tree with a gitignored directory. The
// gitignore exclusion is the entire reason this package exists.
func discoveryFixture(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
		c.NoError(os.WriteFile(p, []byte(body), 0o644))
	}
	write(".gitignore", "ignored/\n")
	write("keep.go", "package main\nconst Needle = 1\n")
	write("nested/also.go", "package nested\nconst Needle = 2\n")
	write("ignored/hidden.go", "package ignored\nconst Needle = 3\n")
	return root
}

func TestDiscoverFilesRespectsGitignore(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	paths, _, err := DiscoverFiles(context.Background(), FileQuery{Root: root})
	c.NoError(err, "DiscoverFiles")
	joined := strings.Join(paths, "\n")
	c.False(!strings.Contains(joined, "keep.go") || !strings.Contains(joined, "also.go"), "expected tracked files, got %v", paths)
	c.NotStrContains(joined, "hidden.go", "gitignored file was returned: %v", paths)
}

func TestDiscoverFilesGlobAndLimit(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	paths, truncated, err := DiscoverFiles(context.Background(), FileQuery{Root: root, Glob: "*.go", Limit: 1})
	c.NoError(err, "DiscoverFiles")
	c.Len(paths, 1, "Limit not honoured: got %d paths", len(paths))
	c.True(truncated, "truncated should be true when Limit cut the result")
}

func TestSearchContentRespectsGitignore(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := discoveryFixture(t)
	matches, _, err := SearchContent(context.Background(), ContentQuery{Root: root, Pattern: "Needle"})
	c.NoError(err, "SearchContent")
	c.Len(matches, 2, "got %d matches, want 2 (the gitignored one must be excluded)", len(matches))
	for _, m := range matches {
		c.False(m.Line == 0 || m.Text == "" || m.Path == "", "incomplete match: %+v", m)
	}
}

// TestSearchContentLimitTruncatesWithoutHangOrError exercises the early
// stdout-close path: with many matches available and a small Limit,
// SearchContent must stop reading, report truncated, and return promptly
// with no error even though it stopped rg mid-stream (via a closed pipe,
// which on this platform surfaces as EPIPE/SIGPIPE to the child).
func TestSearchContentLimitTruncatesWithoutHangOrError(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("const Needle = 1\n")
	}
	c.NoError(os.WriteFile(filepath.Join(root, "big.go"), []byte(sb.String()), 0o644))

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
	c.NoError(err, "SearchContent returned a spurious error on early close")
	c.Len(matches, 3, "got %d matches, want 3", len(matches))
	c.True(truncated, "truncated should be true when Limit cut the result")
}

func TestRgErrorTreatsExitOneAsNoMatches(t *testing.T) {
	c := assert.NewAborting(t)
	if rgPath() == "" {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(root, "f.go"), []byte("nothing here\n"), 0o644))
	matches, truncated, err := SearchContent(context.Background(), ContentQuery{Root: root, Pattern: "NoSuchNeedle"})
	c.NoError(err, "SearchContent: unexpected error for a clean no-match search")
	c.False(truncated, "truncated should be false when nothing matched")
	c.Empty(matches, "got %d matches, want 0", len(matches))
}
