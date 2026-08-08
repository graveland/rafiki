package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditToolSequentialSecondEditSeesFirstResult(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one two three"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Edit 1 replaces "one" with "ONE". Edit 2 replaces "ONE two" with
	// "UNO duo" — old_string "ONE two" only exists AFTER edit 1 has run.
	// In default (non-sequential) mode this would fail because edit 2's
	// old_string doesn't appear in the original content.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"edits":[{"old_string":"one","new_string":"ONE"},{"old_string":"ONE two","new_string":"UNO duo"}],"sequential":true}`, p),
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if got != "UNO duo three" {
		t.Fatalf("sequential edit failed: got %q, want %q", got, "UNO duo three")
	}
}

func TestEditToolSequentialWithoutFlagRejectsChained(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one two three"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Same edits as the sequential test, but without sequential: true.
	// Edit 2's old_string "ONE two" does not exist in the original, so this
	// must fail.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"edits":[{"old_string":"one","new_string":"ONE"},{"old_string":"ONE two","new_string":"UNO duo"}]}`, p),
	))
	if err == nil {
		t.Fatal("expected error for chained old_string in non-sequential mode")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func TestEditToolDefaultModeStillRejectsOverlaps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	readTool := testReadTool(t, tr, "")
	editTool := testEditTool(t, tr, "")
	if _, err := readTool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	// Overlapping edits in default (non-sequential) mode must still be rejected.
	_, err := editTool.Execute(context.Background(), ToolInput(
		fmt.Sprintf(`{"path":%q,"edits":[{"old_string":"hello world","new_string":"hi there"},{"old_string":"world","new_string":"planet"}]}`, p),
	))
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap error, got %v", err)
	}
}
