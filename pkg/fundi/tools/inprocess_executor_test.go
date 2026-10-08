package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// A process that is its own workspace — the standalone rafikid fundi mode —
// satisfies the executor rule with a real client rather than an exemption, so
// there is one rule and nothing routed around it.
func TestInProcessExecutorRunsWorkspaceTools(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	c.Require().NoError(os.WriteFile(path, []byte("hello"), 0o600))

	cl, served := NewInProcessExecutor(ToolOpts{Cwd: dir, FileTracker: NewFileTracker()})
	c.Require().False(!served["read"], "read is not served; the standalone mode would have no file tools")

	input, _ := json.Marshal(map[string]any{"file_path": path})
	outRes, err := cl.Execute(context.Background(), "read", input)
	out := outRes.Text
	c.Require().NoError(err, "Execute(read)")
	c.StrContains(out, "hello", "read returned")
}

// The background-job verbs need a job registry, which lives in pkg/executor and
// this client does not have. They must be absent from the served set rather
// than present and always failing: a tool that can only fail costs the model a
// turn to learn nothing.
func TestInProcessExecutorDoesNotServeJobVerbs(t *testing.T) {
	_, served := NewInProcessExecutor(ToolOpts{Cwd: t.TempDir(), FileTracker: NewFileTracker()})
	for _, name := range []string{"bash_start", "bash_output", "bash_kill"} {
		assert.NewCollecting(t).False(served[name], "%q is advertised but cannot be served in-process", name)
	}
}

// The job verbs must also refuse if called, so a caller that ignores the served
// set gets an answer rather than a panic.
func TestInProcessExecutorRefusesJobCalls(t *testing.T) {
	cl, _ := NewInProcessExecutor(ToolOpts{Cwd: t.TempDir(), FileTracker: NewFileTracker()})
	_, err := cl.StartJob(context.Background(), "true")
	assert.NewCollecting(t).Error(err, "StartJob succeeded; this client has no job registry")
}
