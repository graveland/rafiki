// SPDX-License-Identifier: Apache-2.0

package promptfile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func shaName(t *testing.T, ext, text string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]) + ext
}

// The name is the content hash, so the same text is one file — this is what
// makes Stage's repeated calls over a respawn converge instead of accumulating.
func TestWriteNamesTheFileBySHA(t *testing.T) {
	// A subdirectory of the temp dir, not the temp dir itself: Write must create
	// dir itself (0700), and t.TempDir() may already exist with looser mode.
	dir := filepath.Join(t.TempDir(), "prompts")
	c := assert.NewCollecting(t)
	const text = "be terse and keep going"

	path, err := Write(dir, ".md", text)
	c.Require().NoError(err, "Write")
	c.Eq(shaName(t, ".md", text), filepath.Base(path), "Write basename")
	c.Eq(dir, filepath.Dir(path), "Write dirname")

	got, err := os.ReadFile(path)
	c.Require().NoError(err, "ReadFile")
	c.Eq(text, string(got), "Write content")

	info, err := os.Stat(path)
	c.Require().NoError(err, "Stat(file)")
	c.Eq(os.FileMode(0o600), info.Mode().Perm(), "file mode")

	dirInfo, err := os.Stat(dir)
	c.Require().NoError(err, "Stat(dir)")
	c.Eq(os.FileMode(0o700), dirInfo.Mode().Perm(), "dir mode")
}

// A second write of the same text must return the same path and leave the file
// untouched — pinned by mtime, which the size short-circuit preserves.
func TestWriteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	c := assert.NewCollecting(t)
	const text = "same text twice"

	first, err := Write(dir, ".md", text)
	c.Require().NoError(err, "first Write")

	old := time.Unix(1_000_000, 0)
	c.Require().NoError(os.Chtimes(first, old, old), "Chtimes")

	second, err := Write(dir, ".md", text)
	c.Require().NoError(err, "second Write")
	c.Eq(first, second, "Write path")

	info, err := os.Stat(second)
	c.Require().NoError(err, "Stat")
	c.True(info.ModTime().Equal(old), "mtime changed: want %v, got %v", old, info.ModTime())
}

// A file of the right name at the wrong size is a truncated earlier write; the
// size check must rewrite it rather than trusting the name's existence.
func TestWriteRewritesATruncatedFile(t *testing.T) {
	dir := t.TempDir()
	c := assert.NewCollecting(t)
	const text = "the whole appendix text"

	path := filepath.Join(dir, shaName(t, ".md", text))
	c.Require().NoError(os.WriteFile(path, []byte("short"), 0o600), "seed truncated file")

	got, err := Write(dir, ".md", text)
	c.Require().NoError(err, "Write")
	c.Eq(path, got, "Write path")

	content, err := os.ReadFile(got)
	c.Require().NoError(err, "ReadFile")
	c.Eq(text, string(content), "rewritten content")
}

// The temp-and-rename dance must clean up after itself: no .tmp-* survives a
// successful write.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	c := assert.NewCollecting(t)
	for _, text := range []string{"one", "two", "three", "one"} {
		_, err := Write(dir, ".md", text)
		c.Require().NoError(err, "Write(%q)", text)
	}

	entries, err := os.ReadDir(dir)
	c.Require().NoError(err, "ReadDir")
	for _, e := range entries {
		c.False(strings.HasPrefix(e.Name(), ".tmp-"), "leftover temp file %q", e.Name())
	}
	c.Eq(3, len(entries), "ReadDir: want one file per distinct text, got")
}

// 16 writers of the same text must all land on one path with the full content.
func TestWriteConcurrentSameContent(t *testing.T) {
	dir := t.TempDir()
	c := assert.NewCollecting(t)
	const text = "concurrent writers, one file"

	const n = 16
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = Write(dir, ".md", text)
		}()
	}
	wg.Wait()

	for i := range n {
		c.Require().NoError(errs[i], "Write #%d", i)
		c.Eq(paths[0], paths[i], "Write #%d path", i)
	}
	content, err := os.ReadFile(paths[0])
	c.Require().NoError(err, "ReadFile")
	c.Eq(text, string(content), "content")
}

// A failure before the file lands must be wrapped with the package name so a
// caller can attribute it, and must not leave a temp behind.
func TestWriteFailureWrapsAndCleans(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	c := assert.NewCollecting(t)
	c.Require().NoError(os.WriteFile(blocker, []byte("x"), 0o600), "seed blocker file")

	// dir is under a regular file, so MkdirAll cannot succeed.
	_, err := Write(filepath.Join(blocker, "prompts"), ".md", "text")
	c.Require().Error(err, "Write under a file")
	c.StrContains(err.Error(), "promptfile", "error text")
}
