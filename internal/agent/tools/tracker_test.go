package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileTrackerVerifyUnread(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	err := tr.Verify(p)
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("expected an unread error mentioning 'read', got %v", err)
	}
}

func TestFileTrackerVerifyFreshAfterRecordRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	if err := tr.Verify(p); err != nil {
		t.Fatalf("expected fresh verify to pass, got %v", err)
	}
}

func TestFileTrackerVerifyStaleMtime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())

	// Simulate an out-of-band modification by bumping the mtime forward,
	// deterministically (no sleep-based flakiness).
	newMtime := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(p, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	if err := tr.Verify(p); err == nil {
		t.Fatal("expected staleness error, got nil")
	}
}

func TestFileTrackerVerifyDeletedSinceRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := tr.Verify(p); err == nil {
		t.Fatal("expected an error verifying a deleted file")
	}
}

// TestFileTrackerNormalizesUncleanPath covers the /tmp/x/./a.txt spelling —
// a read recorded under one form must satisfy a verify under another.
func TestFileTrackerNormalizesUncleanPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())

	unclean := filepath.Join(dir, "sub", "..", ".", "a.txt")
	if err := tr.Verify(unclean); err != nil {
		t.Fatalf("expected the uncleaned spelling %q to verify, got %v", unclean, err)
	}
}

// TestFileTrackerNormalizesSymlinkedPath is the macOS /tmp -> /private/tmp
// case: read one spelling, edit the other, and the tracker must not claim the
// file "has not been read yet".
func TestFileTrackerNormalizesSymlinkedPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := filepath.Join(real, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	tr.RecordRead(p, info.ModTime())
	viaLink := filepath.Join(link, "a.txt")
	if err := tr.Verify(viaLink); err != nil {
		t.Fatalf("expected the symlinked spelling %q to verify, got %v", viaLink, err)
	}

	// And in the other direction: record via the link, verify via the real path.
	tr2 := NewFileTracker()
	tr2.RecordRead(viaLink, info.ModTime())
	if err := tr2.Verify(p); err != nil {
		t.Fatalf("expected the real path %q to verify after a symlinked read, got %v", p, err)
	}
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
	if leftover != 0 {
		t.Fatalf("expected the locks map to drain, got %d entries", leftover)
	}
}

// TestNormalizePathIsStableAcrossParentCreation pins the key invariant that
// makes Lock trustworthy for write: the key for a path several directories
// deep must not change when those directories are created underneath it.
// t.TempDir() lives under /var/folders/... on macOS, and /var is a symlink to
// /private/var — so the resolved and unresolved spellings genuinely differ,
// which is the whole point of testing this on real paths.
func TestNormalizePathIsStableAcrossParentCreation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "new.txt")

	before := normalizePath(target)

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	afterMkdir := normalizePath(target)
	if afterMkdir != before {
		t.Fatalf("key changed when the parent directories were created: %q -> %q", before, afterMkdir)
	}

	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterWrite := normalizePath(target)
	if afterWrite != before {
		t.Fatalf("key changed when the file was created: %q -> %q", before, afterWrite)
	}

	// And the stable key must be the fully-resolved one, not the unresolved
	// spelling frozen in — otherwise a later read of the resolved form misses.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if before != resolved {
		t.Fatalf("key %q is not the resolved form %q", before, resolved)
	}
}

// TestFileTrackerLockExcludesAcrossParentCreation is the mutual-exclusion half
// of the same defect: write takes the lock BEFORE os.MkdirAll, so a second
// writer arriving after the directories exist must still land on the same
// mutex. When it didn't, both ran os.WriteFile (O_TRUNC then Write, not
// atomic) on one file concurrently.
func TestFileTrackerLockExcludesAcrossParentCreation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "new.txt")

	tr := NewFileTracker()
	unlock := tr.Lock(target)

	// Exactly what write does next.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

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
	if leftover != 0 {
		t.Fatalf("expected the locks map to drain, got %d entries", leftover)
	}
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
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
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
