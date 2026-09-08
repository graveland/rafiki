package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// Provision exposes the root the executor was started with, and says nothing
// about isolation.
//
// The empty isolation is the assertion, not an oversight: an executor does not
// know whether it is running in a container and must not guess, because the
// answer gates where other people's children may run. The operator's copy is on
// the row. This process reporting "none" is exactly how every sandboxed child
// came to be told it was unsandboxed.
func TestProvisionExposesTheRootAndDeclaresNoIsolation(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	resp, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_1", WorkspaceMode: "pinned",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Isolation != "" {
		t.Errorf("the executor self-reported isolation %q; that fact lives on its row, not in its own answer", resp.Msg.Isolation)
	}
	if len(resp.Msg.Roots) != 1 || resp.Msg.Roots[0] != root {
		t.Errorf("roots = %v, want [%s]", resp.Msg.Roots, root)
	}
}

// Release is idempotent: a daemon restart that lost its workspace table must
// be able to release again without an error it cannot act on.
func TestReleaseIsIdempotent(t *testing.T) {
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	for range 3 {
		if _, err := client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
			WorkspaceId: "never-existed",
		})); err != nil {
			t.Fatalf("Release must be idempotent: %v", err)
		}
	}
}

// Execute with an unknown workspace id must FAIL, not silently fall back to
// the executor's own root. A container child whose workspace vanished must not
// quietly start running against the host.
func TestExecuteWithUnknownWorkspaceFails(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "read",
		InputJson:   []byte(`{"file_path":"` + root + `/nonexistent"}`),
		WorkspaceId: "dead-workspace",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for stream.Receive() {
		if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			failed = true
		}
	}
	_ = stream.Err()
	if !failed {
		t.Fatal("Execute with unknown workspace id must fail")
	}
}

// ...but an EMPTY workspace id is the documented compatibility path.
func TestExecuteWithEmptyWorkspaceUsesTheExecutorRoot(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      "read",
		InputJson: []byte(`{"file_path":"/nonexistent-file"}`),
		// workspace_id empty = executor's own root
	}))
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for stream.Receive() {
		if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			failed = true
		}
	}
	_ = stream.Err()
	// Should fail with tool error (file not found), not "unknown workspace".
	// The executor tried the tool call against its own root.
	if !failed {
		t.Fatal("empty workspace id should route to executor root, yielding tool failure for missing file")
	}
}

// A workspace's tools must start in ITS workdir, not the executor's root.
//
// This is what makes agent_spawn's cwd real for an executor-bound child: the
// daemon sends the child's cwd as ProvisionRequest.workdir, and every tool call
// in that workspace — bash's process directory, the file tools' relative-path
// resolution — must begin there. Before this, bash ran in the executor's root
// while the child's prompt named another directory: a coordinator that pointed
// a worker at a git worktree got an agent whose shell verified and committed in
// the main checkout. The split-brain is the failure; where the tools start is
// the fix.
func TestProvisionHonorsWorkdirAndToolsStartThere(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "wt")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "marker.txt"), []byte("in worktree"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_wt", Workdir: workdir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := prov.Msg.Workdir; got != workdir {
		t.Fatalf("Provision workdir = %q, want %q", got, workdir)
	}
	// roots stay the executor's own view of the machine; the workdir is where
	// tools START, not a claim about what the machine exposes.
	if len(prov.Msg.Roots) != 1 || prov.Msg.Roots[0] != root {
		t.Errorf("roots = %v, want [%s]", prov.Msg.Roots, root)
	}

	if got := strings.TrimSpace(executeText(t, client, ctx, prov.Msg.WorkspaceId, "bash", `{"command":"pwd"}`)); got != workdir {
		t.Errorf("bash pwd = %q, want the workspace workdir %q", got, workdir)
	}
	if got := executeText(t, client, ctx, prov.Msg.WorkspaceId, "read", `{"file_path":"marker.txt"}`); !strings.Contains(got, "in worktree") {
		t.Errorf("read of a relative path = %q, want the worktree's marker.txt", got)
	}

	// A second workspace with a different workdir serves its own cwd: one
	// registry per workspace, and neither leaks into the other.
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	prov2, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_other", Workdir: other,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(executeText(t, client, ctx, prov2.Msg.WorkspaceId, "bash", `{"command":"pwd"}`)); got != other {
		t.Errorf("second workspace's bash pwd = %q, want %q", got, other)
	}
}

// An unset workdir still means the executor's root — the compatibility path
// every pre-workdir daemon takes.
func TestProvisionWithEmptyWorkdirMeansTheRoot(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_root"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := prov.Msg.Workdir; got != root {
		t.Fatalf("empty workdir provisioned as %q, want the root %q", got, root)
	}
}

// A workdir the executor cannot see is REFUSED, never silently swapped for the
// root: "somewhere the child cannot write" is the proto's own refusal rule, and
// starting in the root while the child's prompt names another directory is the
// split-brain this check exists to prevent. On a container executor the same
// check enforces the mount rule — a host path that is not one of the container's
// mounts does not exist in the container's view.
func TestProvisionRejectsAWorkdirItCannotServe(t *testing.T) {
	root := t.TempDir()
	notDir := filepath.Join(root, "file.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		workdir string
	}{
		{"missing directory", filepath.Join(root, "nope")},
		{"relative path", "relative/path"},
		{"a file, not a directory", notDir},
		{"a whitespace path", " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
			client := newTestClient(t, srv)
			_, err := client.Provision(context.Background(), connect.NewRequest(&executorpb.ProvisionRequest{
				ChildId: "c_bad", Workdir: tc.workdir,
			}))
			if err == nil {
				t.Fatalf("workdir %q must be refused", tc.workdir)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("workdir %q: got code %v, want CodeInvalidArgument", tc.workdir, connect.CodeOf(err))
			}
		})
	}
}

// Background jobs start in the workspace's workdir too: a job launched from a
// worktree must not build and commit in the executor's root.
func TestBackgroundJobStartsInTheWorkspaceWorkdir(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "wt")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_job", Workdir: workdir,
	}))
	if err != nil {
		t.Fatal(err)
	}

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "bash",
		InputJson:   []byte(`{"command":"pwd"}`),
		Background:  true,
		WorkspaceId: prov.Msg.WorkspaceId,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var handle string
	for stream.Receive() {
		if h := stream.Msg().GetHandle(); h != "" {
			handle = h
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if handle == "" {
		t.Fatal("background start returned no handle")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{
			Handle: handle,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Msg.Found {
			t.Fatal("job output not found")
		}
		if out.Msg.Exited {
			if got := strings.TrimSpace(string(out.Msg.Data)); got != workdir {
				t.Fatalf("background job pwd = %q, want the workspace workdir %q", got, workdir)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background job never exited")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// executeText runs one tool call in a workspace and returns the result text,
// failing the test on any failure event.
func executeText(t *testing.T, client executorpbconnect.ExecutorServiceClient, ctx context.Context, wsID, tool, input string) string {
	t.Helper()
	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        tool,
		InputJson:   []byte(input),
		WorkspaceId: wsID,
	}))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			var sb strings.Builder
			for _, c := range ev.Result.Content {
				sb.WriteString(c.GetText())
			}
			return sb.String()
		case *executorpb.ExecuteResponse_Failed:
			t.Fatalf("%s failed: %s", tool, ev.Failed.Message)
		}
	}
	t.Fatalf("%s returned no result: %v", tool, stream.Err())
	return ""
}
