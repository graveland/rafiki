package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteToolCreatesNewFileAndParentDirs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deeper", "new.txt")
	tr := NewFileTracker()
	fn := newWriteTool(tr)

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
	fn := newWriteTool(tr)

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
	readFn := newReadTool(tr)
	writeFn := newWriteTool(tr)

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
	fn := newWriteTool(tr)
	_, err := fn(context.Background(), json.RawMessage(`{"path":"rel.txt","content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected an absolute-path error, got %v", err)
	}
}

// TestWriteThenEditWithoutSeparateRead exercises the FileTracker being
// shared: a write should satisfy the tracker for an immediately following
// edit, without a separate read call in between.
func TestWriteThenEditWithoutSeparateRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "roundtrip.txt")
	tr := NewFileTracker()
	writeFn := newWriteTool(tr)
	editFn := newEditTool(tr)

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
