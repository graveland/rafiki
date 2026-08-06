package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteToolCreatesNewFileAndParentDirs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deeper", "new.txt")
	tr := NewFileTracker()
	fn := newWriteTool(tr, "")

	out, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"hello"}`, p)))
	if err != nil {
		t.Fatalf("unexpected error: %v (%s)", err, out)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
}

func TestWriteToolRefusesUnreadExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	fn := newWriteTool(tr, "")

	_, err := fn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"clobber"}`, p)))
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("expected a read-before-write error, got %v", err)
	}
	b, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(b) != "original" {
		t.Fatalf("file should be untouched, got %q", b)
	}
}

func TestWriteToolAllowsOverwriteAfterRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr, "")
	writeFn := newWriteTool(tr, "")

	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"replaced"}`, p))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "replaced" {
		t.Fatalf("content = %q", b)
	}
}

func TestWriteToolRefusesRelativePath(t *testing.T) {
	tr := NewFileTracker()
	fn := newWriteTool(tr, "")
	_, err := fn(context.Background(), json.RawMessage(`{"path":"rel.txt","content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected an absolute-path error, got %v", err)
	}
}

// TestWriteToolConcurrentWritesAreSerialized: writes to one path in a
// concurrently-executed tool batch must not interleave. Whichever write lands
// last, the file must hold exactly one of the payloads verbatim — never a
// torn mix of two — and the tracker must be left consistent enough that a
// following edit needs no fresh read.
func TestWriteToolConcurrentWritesAreSerialized(t *testing.T) {
	const n = 8

	dir := t.TempDir()
	p := filepath.Join(dir, "contended.txt")
	tr := NewFileTracker()
	writeFn := newWriteTool(tr, "")
	editFn := newEditTool(tr, "")

	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = "payload-" + strings.Repeat(fmt.Sprintf("%d", i), 512)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = writeFn(context.Background(), json.RawMessage(
				fmt.Sprintf(`{"path":%q,"content":%q}`, p, payloads[i])))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	matched := false
	for _, want := range payloads {
		if got == want {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final content is not any single payload verbatim (torn write): %q", got)
	}

	if _, err := editFn(context.Background(), json.RawMessage(
		fmt.Sprintf(`{"path":%q,"old_string":"payload-","new_string":"done-"}`, p))); err != nil {
		t.Fatalf("expected the tracker to be left consistent after concurrent writes, got %v", err)
	}
}

// TestWriteToolExcludesASecondWriterAfterParentDirsAppear is the case
// TestWriteToolConcurrentWritesAreSerialized structurally cannot reach:
// t.TempDir() already exists there, so every goroutine computes the same lock
// key and the bug is invisible. Here the parent directories do NOT exist when
// the first writer takes its lock, and DO exist when the second writer takes
// its own — write.go's os.MkdirAll runs between the two.
//
// If the lock key depends on what exists on disk, those two writers get
// different mutexes and the second sails straight through while the first is
// mid-write. os.WriteFile is O_TRUNC then Write, not atomic, so the file can
// end up a torn mix of both payloads. The test stands in for the first writer
// so the interleaving is forced rather than raced for.
func TestWriteToolExcludesASecondWriterAfterParentDirsAppear(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deeper", "contended.txt")
	tr := NewFileTracker()
	writeFn := newWriteTool(tr, "")

	// Writer one: lock first, create the parents second — write.go's order.
	unlock := tr.Lock(p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}

	// Writer two arrives with the directories already on disk.
	done := make(chan error, 1)
	go func() {
		_, err := writeFn(context.Background(), json.RawMessage(
			fmt.Sprintf(`{"path":%q,"content":"second"}`, p)))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the second write was not excluded by the first writer's lock (err=%v); both would run a non-atomic os.WriteFile on %s at once", err, p)
	case <-time.After(100 * time.Millisecond):
	}

	// Finish writer one's job under its own lock, then hand over.
	if err := os.WriteFile(p, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tr.RecordRead(p, info.ModTime())
	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second write failed once the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second write never completed after the lock was released")
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second" {
		t.Fatalf("expected the serialized second write to land intact, got %q", b)
	}
}

// TestWriteToolConcurrentWritesIntoUncreatedDirsAreSerialized runs the real
// burst — n concurrent write tools into a path whose parent directories don't
// exist — as a race-detector and regression guard over the same defect. The
// payloads deliberately differ in length: two equal-length writes at offset 0
// can only ever leave one of them intact, whereas a short write landing after
// a long one leaves a visibly torn file.
func TestWriteToolConcurrentWritesIntoUncreatedDirsAreSerialized(t *testing.T) {
	const n = 8

	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deeper", "contended.txt")
	tr := NewFileTracker()
	writeFn := newWriteTool(tr, "")

	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = "payload-" + strings.Repeat(fmt.Sprintf("%d", i), (i+1)*40000)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = writeFn(context.Background(), json.RawMessage(
				fmt.Sprintf(`{"path":%q,"content":%q}`, p, payloads[i])))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range payloads {
		if got == want {
			return
		}
	}
	t.Fatalf("final content is not any single payload verbatim (torn write): %d bytes, prefix %q", len(got), got[:min(64, len(got))])
}

// TestWriteThenEditWithoutSeparateRead exercises the FileTracker being
// shared: a write should satisfy the tracker for an immediately following
// edit, without a separate read call in between.
func TestWriteThenEditWithoutSeparateRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "roundtrip.txt")
	tr := NewFileTracker()
	writeFn := newWriteTool(tr, "")
	editFn := newEditTool(tr, "")

	if _, err := writeFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"hello world"}`, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"bye"}`, p))); err != nil {
		t.Fatalf("expected edit to succeed right after write without a read, got %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "bye world" {
		t.Fatalf("content = %q", b)
	}
}
