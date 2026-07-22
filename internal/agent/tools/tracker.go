package tools

import (
	"fmt"
	"os"
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
type FileTracker struct {
	mu    sync.Mutex
	reads map[string]time.Time
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{reads: make(map[string]time.Time)}
}

// RecordRead notes that path was observed with the given on-disk mtime.
// Callers record after every successful read, write, and edit — a tool's
// own change satisfies the tracker for whatever runs next, so a write
// immediately followed by an edit doesn't need a redundant read in between.
func (t *FileTracker) RecordRead(path string, mtime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads[path] = mtime
}

// Verify returns nil only if path was previously recorded AND its on-disk
// mtime still matches what was recorded. A path with no recorded read fails
// with an error mentioning "read" (write.go/edit.go and their tests key off
// that word to distinguish "never read" from "read, but now stale").
func (t *FileTracker) Verify(path string) error {
	t.mu.Lock()
	recorded, ok := t.reads[path]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s has not been read yet; read it before writing to it", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s could not be re-checked before writing (it may have been deleted since it was last read): %w", path, err)
	}
	if !info.ModTime().Equal(recorded) {
		return fmt.Errorf("%s was modified on disk since it was last read (expected mtime %s, found %s); read it again before writing", path, recorded, info.ModTime())
	}
	return nil
}
