package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process that is its own workspace — the standalone rafikid fundi mode —
// satisfies the executor rule with a real client rather than an exemption, so
// there is one rule and nothing routed around it.
func TestInProcessExecutorRunsWorkspaceTools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cl, served := NewInProcessExecutor(ToolOpts{Cwd: dir, FileTracker: NewFileTracker()})
	if !served["read"] {
		t.Fatal("read is not served; the standalone mode would have no file tools")
	}

	input, _ := json.Marshal(map[string]any{"file_path": path})
	out, err := cl.Execute(context.Background(), "read", input)
	if err != nil {
		t.Fatalf("Execute(read): %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("read returned %q, want the file's contents", out)
	}
}

// The background-job verbs need a job registry, which lives in pkg/executor and
// this client does not have. They must be absent from the served set rather
// than present and always failing: a tool that can only fail costs the model a
// turn to learn nothing.
func TestInProcessExecutorDoesNotServeJobVerbs(t *testing.T) {
	_, served := NewInProcessExecutor(ToolOpts{Cwd: t.TempDir(), FileTracker: NewFileTracker()})
	for _, name := range []string{"bash_start", "bash_output", "bash_kill"} {
		if served[name] {
			t.Errorf("%q is advertised but cannot be served in-process", name)
		}
	}
}

// The job verbs must also refuse if called, so a caller that ignores the served
// set gets an answer rather than a panic.
func TestInProcessExecutorRefusesJobCalls(t *testing.T) {
	cl, _ := NewInProcessExecutor(ToolOpts{Cwd: t.TempDir(), FileTracker: NewFileTracker()})
	if _, err := cl.StartJob(context.Background(), "true"); err == nil {
		t.Error("StartJob succeeded; this client has no job registry")
	}
}
