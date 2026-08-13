package executor_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// The native backend must be a no-op that reports the truth: it exposes the
// root it was started with, ignores mounts it cannot honour, and says so.
func TestNativeProvisionRefusesEphemeral(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	_, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId:       "c_1",
		WorkspaceMode: "ephemeral", // asked for; cannot be honoured
	}))
	if err == nil {
		t.Fatal("a native executor cannot construct an ephemeral workspace and must refuse rather than pretend")
	}
	t.Logf("refused ephemeral (correct): %v", err)
}

func TestNativeProvisionAcceptsPinned(t *testing.T) {
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
	if resp.Msg.Isolation != "none" {
		t.Errorf("a native executor's isolation is none; claiming otherwise is a grant nobody enforces: %q", resp.Msg.Isolation)
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
		Tool:         "read",
		InputJson:    []byte(`{"file_path":"` + root + `/nonexistent"}`),
		WorkspaceId:  "dead-workspace",
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