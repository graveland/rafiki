package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileTracker records the on-disk mtime observed by the last successful
// read of a path, and is consulted by write and edit before they touch a
// file. It is the read-before-write guard: a file must be read (and not
// modified on disk since) before it can be edited or overwritten, so the
// agent never clobbers a change it hasn't seen.
//
// One FileTracker is shared across read, write, and edit — and, because
// rafiki's agentloop executes a tool batch concurrently under an errgroup
// (see RegisterFileTools), it is touched from multiple goroutines at once.
// All access is mutex-guarded.
//
// Verify alone is not enough for concurrent safety: it reports whether a path
// is fresh, it does not reserve it. Two edits on the same file in one batch
// would both Verify, both read the pre-state, and the second write would
// silently discard the first. FileTracker therefore also hands out per-path
// mutual exclusion via Lock, which write and edit hold across their whole
// verify -> read -> modify -> write -> record sequence.
type FileTracker struct {
	mu    sync.Mutex
	reads map[string]time.Time
	locks map[string]*pathLock
}

// pathLock is the per-path mutex handed out by Lock, plus a refcount so the
// locks map doesn't grow without bound over a long session.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{
		reads: make(map[string]time.Time),
		locks: make(map[string]*pathLock),
	}
}

// normalizePath collapses the many spellings of one file into a single map
// key. Without it, read("/tmp/x/a.txt") followed by
// edit("/private/tmp/x/a.txt") — the same file on macOS, where /tmp and /var
// are symlinks — would miss the map and report "has not been read yet" for a
// file that was just read.
//
// Symlink resolution is best-effort by design: the file may legitimately not
// exist yet (write creating a new file), in which case the parent directory
// is resolved instead so the pre-create and post-create keys agree.
func normalizePath(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	if dir, file := filepath.Split(clean); dir != "" {
		if resolvedDir, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
			return filepath.Join(resolvedDir, file)
		}
	}
	// Neither the file nor its parent directory exists yet (write into a
	// directory it is about to create). Clean alone is a consistent key for
	// that case, and becomes the resolved form once the path materializes.
	slog.Debug("agent/tools: path does not resolve to a real path yet, keying on its cleaned form", "path", path, "key", clean)
	return clean
}

// Lock takes the per-path mutex for path and returns the function that
// releases it. Callers that read-modify-write a file MUST hold it across the
// entire sequence — Verify, the read, the computation, the write, and the
// RecordRead — or a concurrent tool in the same batch can interleave and have
// its change silently discarded. The returned closure is idempotent.
//
// Keying uses the same normalization as RecordRead and Verify, so the lock
// and the record can never disagree about which file they refer to.
func (t *FileTracker) Lock(path string) func() {
	key := normalizePath(path)

	t.mu.Lock()
	l, ok := t.locks[key]
	if !ok {
		l = &pathLock{}
		t.locks[key] = l
	}
	l.refs++
	t.mu.Unlock()

	l.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			t.mu.Lock()
			l.refs--
			if l.refs == 0 {
				delete(t.locks, key)
			}
			t.mu.Unlock()
		})
	}
}

// RecordRead notes that path was observed with the given on-disk mtime.
// Callers record after every successful read, write, and edit — a tool's
// own change satisfies the tracker for whatever runs next, so a write
// immediately followed by an edit doesn't need a redundant read in between.
func (t *FileTracker) RecordRead(path string, mtime time.Time) {
	key := normalizePath(path)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads[key] = mtime
}

// Verify returns nil only if path was previously recorded AND its on-disk
// mtime still matches what was recorded. A path with no recorded read fails
// with an error mentioning "read" (write.go/edit.go and their tests key off
// that word to distinguish "never read" from "read, but now stale").
//
// Verify only reports freshness; it does not reserve the path. Callers that
// go on to modify the file must hold Lock(path) around this call and the
// modification — see Lock.
func (t *FileTracker) Verify(path string) error {
	key := normalizePath(path)
	t.mu.Lock()
	recorded, ok := t.reads[key]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s has not been read yet; read it before writing to it", path)
	}
	info, err := os.Stat(key)
	if err != nil {
		return fmt.Errorf("%s could not be re-checked before writing (it may have been deleted since it was last read): %w", path, err)
	}
	if !info.ModTime().Equal(recorded) {
		return fmt.Errorf("%s was modified on disk since it was last read (expected mtime %s, found %s); read it again before writing", path, recorded, info.ModTime())
	}
	return nil
}
