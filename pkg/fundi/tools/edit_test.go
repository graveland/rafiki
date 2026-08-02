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

func TestEditToolNoMatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"nope","new_string":"x"}`, p)))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestEditToolMultipleMatchesWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("foo foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	_, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"foo","new_string":"bar"}`, p)))
	if err == nil || !strings.Contains(err.Error(), "3") {
		t.Fatalf("expected an error mentioning the 3 matches, got %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "foo foo foo" {
		t.Fatalf("file should be untouched, got %q", b)
	}
}

func TestEditToolReplaceAll(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("foo foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"foo","new_string":"bar","replace_all":true}`, p))); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "bar bar bar" {
		t.Fatalf("content = %q", b)
	}
}

func TestEditToolStaleMtime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}

	// Out-of-band modification after the read, with a deterministically
	// bumped mtime (no sleep-based flakiness).
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hello mars"), 0o644); err != nil {
		t.Fatal(err)
	}
	newMtime := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(p, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	_, err = editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"bye"}`, p)))
	if err == nil {
		t.Fatal("expected a staleness error")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello mars" {
		t.Fatalf("file should be untouched by the rejected edit, got %q", b)
	}
}

func TestEditToolChainedEditsNeedOnlyOneRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one two three"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"one","new_string":"1"}`, p))); err != nil {
		t.Fatal(err)
	}
	// Second edit relies on the first edit's own RecordRead, not a fresh read.
	if _, err := editFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"two","new_string":"2"}`, p))); err != nil {
		t.Fatalf("expected chained edit to succeed, got %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "1 2 three" {
		t.Fatalf("content = %q", b)
	}
}

// TestEditToolConcurrentEditsAreNotLost is the regression test for the
// read-modify-write race: rafiki's agentloop runs a tool batch concurrently
// (errgroup, SetLimit(6)), and a model emitting several edits on one file in
// one batch is routine. Without per-path locking in FileTracker every
// goroutine verifies, reads the same pre-state, and the last write wins —
// silently discarding the others while reporting success to the model.
func TestEditToolConcurrentEditsAreNotLost(t *testing.T) {
	const n = 8

	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("t%d", i)
	}
	if err := os.WriteFile(p, []byte(strings.Join(tokens, " ")), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	readFn := newReadTool(tr)
	editFn := newEditTool(tr)
	if _, err := readFn(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = editFn(context.Background(), json.RawMessage(
				fmt.Sprintf(`{"path":%q,"old_string":"t%d","new_string":"X%d"}`, p, i, i)))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("edit %d failed: %v", i, err)
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("X%d", i)
		if !strings.Contains(got, want) {
			t.Errorf("edit %d was silently lost: %q missing from %q", i, want, got)
		}
	}
}

func TestEditToolRelativePathRejected(t *testing.T) {
	tr := NewFileTracker()
	editFn := newEditTool(tr)
	_, err := editFn(context.Background(), json.RawMessage(`{"path":"rel.txt","old_string":"a","new_string":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected an absolute-path error, got %v", err)
	}
}
