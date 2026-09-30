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

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	resp, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_1", WorkspaceMode: "pinned",
	}))
	c.Require().NoError(err)
	c.Eq("", resp.Msg.Isolation, "the executor self-reported isolation")
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
		_, err := client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
			WorkspaceId: "never-existed",
		}))
		assert.NewAborting(t).NoError(err, "Release must be idempotent")
	}
}

// Execute with an unknown workspace id must FAIL, not silently fall back to
// the executor's own root. A container child whose workspace vanished must not
// quietly start running against the host.
func TestExecuteWithUnknownWorkspaceFails(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "read",
		InputJson:   []byte(`{"file_path":"` + root + `/nonexistent"}`),
		WorkspaceId: "dead-workspace",
	}))
	c.NoError(err)
	var failed bool
	for stream.Receive() {
		if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			failed = true
		}
	}
	_ = stream.Err()
	c.True(failed, "Execute with unknown workspace id must fail")
}

// ...but an EMPTY workspace id is the documented compatibility path.
func TestExecuteWithEmptyWorkspaceUsesTheExecutorRoot(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      "read",
		InputJson: []byte(`{"file_path":"/nonexistent-file"}`),
		// workspace_id empty = executor's own root
	}))
	c.NoError(err)
	var failed bool
	for stream.Receive() {
		if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			failed = true
		}
	}
	_ = stream.Err()
	// Should fail with tool error (file not found), not "unknown workspace".
	// The executor tried the tool call against its own root.
	c.True(failed, "empty workspace id should route to executor root, yielding tool failure for missing file")
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
	c := assert.NewCollecting(t)
	root := t.TempDir()
	workdir := filepath.Join(root, "wt")
	c.Require().NoError(os.MkdirAll(workdir, 0o755))
	c.Require().NoError(os.WriteFile(filepath.Join(workdir, "marker.txt"), []byte("in worktree"), 0o644))
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_wt", Workdir: workdir,
	}))
	c.Require().NoError(err)
	c.Require().Eq(workdir, prov.Msg.Workdir, "Provision workdir")
	// roots stay the executor's own view of the machine; the workdir is where
	// tools START, not a claim about what the machine exposes.
	if len(prov.Msg.Roots) != 1 || prov.Msg.Roots[0] != root {
		t.Errorf("roots = %v, want [%s]", prov.Msg.Roots, root)
	}

	c.Eq(workdir, strings.TrimSpace(executeText(t, client, ctx, prov.Msg.WorkspaceId, "bash", `{"command":"pwd"}`)), "bash pwd")
	c.StrContains(executeText(t, client, ctx, prov.Msg.WorkspaceId, "read", `{"file_path":"marker.txt"}`), "in worktree", "read of a relative path")

	// A second workspace with a different workdir serves its own cwd: one
	// registry per workspace, and neither leaks into the other.
	other := filepath.Join(root, "other")
	c.Require().NoError(os.MkdirAll(other, 0o755))
	prov2, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_other", Workdir: other,
	}))
	c.Require().NoError(err)
	c.Eq(other, strings.TrimSpace(executeText(t, client, ctx, prov2.Msg.WorkspaceId, "bash", `{"command":"pwd"}`)), "second workspace's bash pwd")
}

// An unset workdir still means the executor's root — the compatibility path
// every pre-workdir daemon takes.
func TestProvisionWithEmptyWorkdirMeansTheRoot(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_root"}))
	c.NoError(err)
	c.Eq(root, prov.Msg.Workdir, "empty workdir provisioned as")
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
	assert.NewAborting(t).NoError(os.WriteFile(notDir, []byte("x"), 0o644))
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
			c := assert.NewCollecting(t)
			srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
			client := newTestClient(t, srv)
			_, err := client.Provision(context.Background(), connect.NewRequest(&executorpb.ProvisionRequest{
				ChildId: "c_bad", Workdir: tc.workdir,
			}))
			c.Require().Error(err, "workdir %q must be refused", tc.workdir)
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "workdir %q: got code %v, want CodeInvalidArgument", tc.workdir, connect.CodeOf(err))
		})
	}
}

// Background jobs start in the workspace's workdir too: a job launched from a
// worktree must not build and commit in the executor's root.
func TestBackgroundJobStartsInTheWorkspaceWorkdir(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	workdir := filepath.Join(root, "wt")
	c.NoError(os.MkdirAll(workdir, 0o755))
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId: "c_job", Workdir: workdir,
	}))
	c.NoError(err)

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "bash",
		InputJson:   []byte(`{"command":"pwd"}`),
		Background:  true,
		WorkspaceId: prov.Msg.WorkspaceId,
	}))
	c.NoError(err)
	var handle string
	for stream.Receive() {
		if h := stream.Msg().GetHandle(); h != "" {
			handle = h
		}
	}
	c.NoError(stream.Err())
	c.NotEq("", handle, "background start returned no handle")

	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{
			Handle: handle,
		}))
		c.NoError(err)
		c.True(out.Msg.Found, "job output not found")
		if out.Msg.Exited {
			c.Eq(workdir, strings.TrimSpace(string(out.Msg.Data)), "background job pwd")
			return
		}
		c.False(time.Now().After(deadline), "background job never exited")
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
	assert.NewAborting(t).NoError(err, "%s", tool)
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

// A workspace's tool shells carry the agent markers for THEIR child: AI_AGENT
// says an agent is driving, RAFIKI_CHILD_ID names which one. Both are applied
// last, so a stale copy in the executor's pinned environment cannot shadow
// them, and two workspaces never see each other's id.
func TestProvisionedToolShellsCarryAgentMarkers(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{
		Root: root, Version: "test",
		Env: append(os.Environ(), "AI_AGENT=stale", "RAFIKI_CHILD_ID=stale"),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	show := `{"command":"echo \"$AI_AGENT $RAFIKI_CHILD_ID\""}`
	for _, id := range []string{"c_first", "c_second"} {
		prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{ChildId: id}))
		c.Require().NoError(err)
		c.Eq("rafiki "+id, strings.TrimSpace(executeText(t, client, ctx, prov.Msg.WorkspaceId, "bash", show)), "markers for "+id)
	}
}
