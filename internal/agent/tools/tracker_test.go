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
