package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestFileTrackerVerifyUnread(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	tr := NewFileTracker()
	err := tr.Verify(p)
	c.False(err == nil || !strings.Contains(err.Error(), "read"), "expected an unread error mentioning 'read', got %v", err)
}

func TestFileTrackerVerifyFreshAfterRecordRead(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	info, err := os.Stat(p)
	c.NoError(err)
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	c.NoError(tr.Verify(p), "expected fresh verify to pass, got")
}

func TestFileTrackerVerifyStaleMtime(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	info, err := os.Stat(p)
	c.NoError(err)
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())

	// Simulate an out-of-band modification by bumping the mtime forward,
	// deterministically (no sleep-based flakiness).
	newMtime := info.ModTime().Add(2 * time.Second)
	c.NoError(os.Chtimes(p, newMtime, newMtime))

	c.Error(tr.Verify(p), "expected staleness error, got nil")
}

func TestFileTrackerVerifyDeletedSinceRead(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	info, err := os.Stat(p)
	c.NoError(err)
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	c.NoError(os.Remove(p))
	c.Error(tr.Verify(p), "expected an error verifying a deleted file")
}

// TestFileTrackerNormalizesUncleanPath covers the /tmp/x/./a.txt spelling —
// a read recorded under one form must satisfy a verify under another.
func TestFileTrackerNormalizesUncleanPath(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	info, err := os.Stat(p)
	c.NoError(err)
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())

	unclean := filepath.Join(dir, "sub", "..", ".", "a.txt")
	c.NoError(tr.Verify(unclean), "expected the uncleaned spelling %q to verify, got", unclean)
}

// TestFileTrackerNormalizesSymlinkedPath is the macOS /tmp -> /private/tmp
// case: read one spelling, edit the other, and the tracker must not claim the
// file "has not been read yet".
func TestFileTrackerNormalizesSymlinkedPath(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	c.NoError(os.MkdirAll(real, 0o755))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := filepath.Join(real, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello"), 0o644))
	info, err := os.Stat(p)
	c.NoError(err)

	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	viaLink := filepath.Join(link, "a.txt")
	c.NoError(tr.Verify(viaLink), "expected the symlinked spelling %q to verify, got", viaLink)

	// And in the other direction: record via the link, verify via the real path.
	tr2 := NewFileTracker()
	tr2.RecordRead(viaLink, info.ModTime())
	c.NoError(tr2.Verify(p), "expected the real path %q to verify after a symlinked read, got", p)
}

// TestFileTrackerLockIsPerPath checks that Lock excludes on the same path,
// keys through the same normalization as RecordRead/Verify, and does not
// serialize unrelated paths.
func TestFileTrackerLockIsPerPath(t *testing.T) {
	dir := t.TempDir()
	tr := NewFileTracker()

	a := filepath.Join(dir, "a.txt")
	unlockA := tr.Lock(a)

	// A different path must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Lock(filepath.Join(dir, "b.txt"))()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock on an unrelated path blocked")
	}

	// The same path spelled differently must block until unlockA runs.
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		tr.Lock(filepath.Join(dir, "sub", "..", "a.txt"))()
	}()
	select {
	case <-blocked:
		t.Fatal("Lock did not exclude a differently-spelled form of the same path")
	case <-time.After(50 * time.Millisecond):
	}

	unlockA()
	unlockA() // idempotent: a second release must not panic or double-unlock.

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock was not released")
	}

	// Refcounting must clean up: no leftover entries once everything unlocks.
	tr.mu.Lock()
	leftover := len(tr.locks)
	tr.mu.Unlock()
	assert.NewAborting(t).Eq(0, leftover, "expected the locks map to drain, got")
}

// TestNormalizePathIsStableAcrossParentCreation pins the key invariant that
// makes Lock trustworthy for write: the key for a path several directories
// deep must not change when those directories are created underneath it.
// t.TempDir() lives under /var/folders/... on macOS, and /var is a symlink to
// /private/var — so the resolved and unresolved spellings genuinely differ,
// which is the whole point of testing this on real paths.
func TestNormalizePathIsStableAcrossParentCreation(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "new.txt")

	before := normalizePath(target)

	c.NoError(os.MkdirAll(filepath.Dir(target), 0o755))
	afterMkdir := normalizePath(target)
	c.Eq(before, afterMkdir, "key changed when the parent directories were created")

	c.NoError(os.WriteFile(target, []byte("hello"), 0o644))
	afterWrite := normalizePath(target)
	c.Eq(before, afterWrite, "key changed when the file was created")

	// And the stable key must be the fully-resolved one, not the unresolved
	// spelling frozen in — otherwise a later read of the resolved form misses.
	resolved, err := filepath.EvalSymlinks(target)
	c.NoError(err)
	c.Eq(resolved, before, "key")
}

// TestFileTrackerLockExcludesAcrossParentCreation is the mutual-exclusion half
// of the same defect: write takes the lock BEFORE os.MkdirAll, so a second
// writer arriving after the directories exist must still land on the same
// mutex. When it didn't, both ran os.WriteFile (O_TRUNC then Write, not
// atomic) on one file concurrently.
func TestFileTrackerLockExcludesAcrossParentCreation(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "new.txt")

	tr := NewFileTracker()
	unlock := tr.Lock(target)

	// Exactly what write does next.
	c.NoError(os.MkdirAll(filepath.Dir(target), 0o755))

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		tr.Lock(target)()
	}()
	select {
	case <-blocked:
		t.Fatal("Lock stopped excluding once the parent directories were created")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Lock was not released")
	}

	tr.mu.Lock()
	leftover := len(tr.locks)
	tr.mu.Unlock()
	c.Eq(0, leftover, "expected the locks map to drain, got")
}

// TestFileTrackerConcurrentAccess hammers RecordRead/Verify from many
// goroutines — read/write/edit tools execute concurrently under the loop's
// errgroup and all share one FileTracker. Run with -race.
func TestFileTrackerConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	tr := NewFileTracker()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		p := filepath.Join(dir, string(rune('a'+i))+".txt")
		assert.NewAborting(t).NoError(os.WriteFile(p, []byte("x"), 0o644))
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				info, err := os.Stat(path)
				if err != nil {
					t.Error(err)
					return
				}
				tr.RecordRead(path, info.ModTime())
				_ = tr.Verify(path)
			}
		}(p)
	}
	wg.Wait()
}
