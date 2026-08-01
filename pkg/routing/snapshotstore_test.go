// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileSnapshotStoreRoundTrip(t *testing.T) {
	s := FileSnapshotStore{Path: filepath.Join(t.TempDir(), "nested", "dir", "catalog.json")}

	if _, err := s.Load(); !os.IsNotExist(err) {
		t.Fatalf("Load on a cold cache: want os.ErrNotExist, got %v", err)
	}

	want := []byte(`{"data":[{"id":"openai/gpt-5-codex"}]}`)
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err) // must create the parent directories itself
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip: got %q, want %q", got, want)
	}

	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// CreateTemp makes 0600; the snapshot is not a secret and other tools on
	// the machine read it, so the rename must land 0644.
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode: got %v, want 0644", perm)
	}
}

func TestFileSnapshotStoreOverwrite(t *testing.T) {
	s := FileSnapshotStore{Path: filepath.Join(t.TempDir(), "catalog.json")}
	if err := s.Save([]byte("first-and-much-longer-than-the-second")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save([]byte("second")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A rename replaces rather than overlays: no tail of the longer first
	// write may survive.
	if string(got) != "second" {
		t.Errorf("overwrite: got %q, want %q", got, "second")
	}
}

// TestFileSnapshotStoreNoTornReads is the reason Save renames instead of
// writing in place. Concurrent readers must see either the old snapshot or the
// new one, never a truncated or half-written file — a torn read decodes as a
// JSON error and yields a silently empty catalog.
func TestFileSnapshotStoreNoTornReads(t *testing.T) {
	s := FileSnapshotStore{Path: filepath.Join(t.TempDir(), "catalog.json")}
	a := []byte(`{"data":[` + string(make([]byte, 4096)) + `]}`)
	b := []byte(`{"data":[]}`)
	if err := s.Save(a); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // writer: flip between two very different sizes
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			payload := a
			if i%2 == 0 {
				payload = b
			}
			if err := s.Save(payload); err != nil {
				t.Errorf("concurrent Save: %v", err)
				return
			}
		}
	}()

	for range 400 { // reader: every observation must be one whole snapshot
		got, err := s.Load()
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("concurrent Load: %v", err)
		}
		if string(got) != string(a) && string(got) != string(b) {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: got %d bytes, want either %d or %d", len(got), len(a), len(b))
		}
	}
	close(stop)
	wg.Wait()
}

// TestFileSnapshotStoreLeavesNoTempFiles guards the cleanup: completion runs on
// every TAB press, so a leaked temp per write would quietly fill the cache dir.
func TestFileSnapshotStoreLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := FileSnapshotStore{Path: filepath.Join(dir, "catalog.json")}
	for range 5 {
		if err := s.Save([]byte("x")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 1 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("want only the snapshot, got %v", names)
	}
}
