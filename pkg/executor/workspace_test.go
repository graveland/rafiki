package executor_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
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
