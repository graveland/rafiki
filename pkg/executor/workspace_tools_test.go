package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// execTool runs any tool in a workspace and reports the result text and the
// failure message separately — a tool that REFUSES is a Failed event, and a
// test that folds the two together cannot tell a refusal from a success.
func execTool(t *testing.T, srv *executor.Server, wsID, tool string, input any) (result, failure string) {
	t.Helper()
	client := newTestClient(t, srv)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        tool,
		InputJson:   raw,
		WorkspaceId: wsID,
	}))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				result += c.GetText()
			}
		case *executorpb.ExecuteResponse_Failed:
			failure = ev.Failed.Message
		}
	}
	if e := stream.Err(); e != nil {
		t.Fatalf("%s stream: %v", tool, e)
	}
	return result, failure
}

// skipUntilInContainerToolServer parks the tests below, LOUDLY.
//
// They are not aspirational: run them today and they FAIL, because a container
// workspace routes only `bash` into the container and sends the other five
// tools to an in-process registry rooted on the host. That is finding D1, it is
// a live host-filesystem escape, and these tests demonstrate it — `read` of
// "~/.ssh/id_rsa" through a container workspace returns the executor host's
// private key.
//
// The fix is not a userspace path check on the host (see the plan for why the
// review's D1 misreads 08-containers.md) but a tool server running INSIDE the
// container. Until that lands they are kept, and kept visible, rather than
// deleted: the knowledge is the point.
//
//	docs/plans/2026-08-15-in-container-tool-server-plan.md
func skipUntilInContainerToolServer(t *testing.T) {
	t.Helper()
	fmt.Fprintf(os.Stderr,
		"\n!!! SKIPPING %s: container workspaces run only `bash` in the container;\n"+
			"!!! read/write/edit/glob/grep still execute on the HOST (finding D1, UNFIXED).\n"+
			"!!! See docs/plans/2026-08-15-in-container-tool-server-plan.md\n\n",
		t.Name())
	t.Skip("D1 unfixed: file tools bypass the container (see the in-container tool server plan)")
}

// The grant must hold for EVERY tool, not just bash. container_test.go
// exercises only execBash, which is why this shipped: the container argv, the
// :ro mount and the kernel enforcement were all correct, and the other five
// tools simply went somewhere else — an in-process registry rooted on the
// HOST, with no mount table and no containment.
func TestFileToolsCannotEscapeTheWorkspace(t *testing.T) {
	skipUntilInContainerToolServer(t)
	requireDocker(t)
	repo, worktree := gitRepoWithDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, worktree, repo)

	// A file on the host that must remain unreachable, standing in for
	// ~/.aws/credentials. Placed outside every mount.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key = hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		tool  string
		input any
	}{
		{"read a host absolute path", "read", map[string]string{"file_path": secret}},
		{"read /etc/passwd", "read", map[string]string{"file_path": "/etc/passwd"}},
		{"read via ~ expansion", "read", map[string]string{"file_path": "~/.ssh/id_rsa"}},
		{"read via .. traversal", "read", map[string]string{"file_path": "/work/../../etc/passwd"}},
		{"write outside every mount", "write", map[string]any{
			"file_path": filepath.Join(secretDir, "planted"), "content": "x"}},
		{"write through ~", "write", map[string]any{"file_path": "~/.zshenv", "content": "x"}},
		{"write through a read-only mount", "write", map[string]any{
			"file_path": "/repo/planted", "content": "x"}},
		{"edit through a read-only mount", "edit", map[string]any{
			"file_path": "/repo/README.md", "old_string": "# test", "new_string": "# owned"}},
		{"glob the host root", "glob", map[string]string{"pattern": "*", "path": "/etc"}},
		{"grep the host root", "grep", map[string]string{"pattern": "root", "path": "/etc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, failure := execTool(t, srv, ws.WorkspaceId, tc.tool, tc.input)
			if failure == "" {
				t.Fatalf("the call SUCCEEDED and must not have; result: %s", truncate(result, 300))
			}
			// A refusal the model cannot act on is a refusal it will retry.
			if !strings.Contains(strings.ToLower(failure), "workspace") {
				t.Errorf("the refusal must say the path is outside the workspace: %s", failure)
			}
		})
	}

	// And the secret is still exactly as it was — no partial write got through.
	if b, err := os.ReadFile(secret); err != nil || !strings.Contains(string(b), "hunter2") {
		t.Errorf("the host file was modified: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(secretDir, "planted")); err == nil {
		t.Error("a write landed outside every mount")
	}
	if _, err := os.Stat(filepath.Join(repo, "planted")); err == nil {
		t.Error("a write landed through the read-only mount")
	}
}

// The honest other half. Container mode is not merely leaky for the file
// tools, it is NON-FUNCTIONAL: the registry is rooted on the host, where
// /work does not exist at all, so every legitimate call fails too. A fix that
// only tightened the refusals would leave the feature broken.
func TestFileToolsWorkOnWorkspacePaths(t *testing.T) {
	skipUntilInContainerToolServer(t)
	requireDocker(t)
	repo, worktree := gitRepoWithDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, worktree, repo)

	if _, failure := execTool(t, srv, ws.WorkspaceId, "write", map[string]any{
		"file_path": "/work/hello.txt", "content": "hello from the workspace\n",
	}); failure != "" {
		t.Fatalf("writing inside the workspace must succeed: %s", failure)
	}

	// It must appear at the corresponding path on the HOST — the mount is a
	// bijection, and a write that does not land there wrote somewhere else.
	onHost := filepath.Join(worktree, "hello.txt")
	b, err := os.ReadFile(onHost)
	if err != nil {
		t.Fatalf("the write did not land in the worktree: %v", err)
	}
	if !strings.Contains(string(b), "hello from the workspace") {
		t.Fatalf("worktree file has %q", b)
	}

	result, failure := execTool(t, srv, ws.WorkspaceId, "read", map[string]string{"file_path": "/work/hello.txt"})
	if failure != "" {
		t.Fatalf("reading it back must succeed: %s", failure)
	}
	if !strings.Contains(result, "hello from the workspace") {
		t.Fatalf("read returned %q", truncate(result, 200))
	}

	// A relative path resolves against the workspace workdir, not the
	// executor's own root.
	result, failure = execTool(t, srv, ws.WorkspaceId, "read", map[string]string{"file_path": "hello.txt"})
	if failure != "" || !strings.Contains(result, "hello from the workspace") {
		t.Fatalf("a relative path must resolve inside the workspace: %s %s", failure, truncate(result, 200))
	}

	result, failure = execTool(t, srv, ws.WorkspaceId, "glob", map[string]string{"pattern": "*.txt", "path": "/work"})
	if failure != "" {
		t.Fatalf("glob inside the workspace must succeed: %s", failure)
	}
	if !strings.Contains(result, "hello.txt") {
		t.Fatalf("glob over /work did not find the file: %s", truncate(result, 300))
	}

	result, failure = execTool(t, srv, ws.WorkspaceId, "grep", map[string]string{
		"pattern": "hello from", "path": "/work",
	})
	if failure != "" {
		t.Fatalf("grep inside the workspace must succeed: %s", failure)
	}
	if !strings.Contains(result, "hello.txt") {
		t.Fatalf("grep over /work did not find the file: %s", truncate(result, 300))
	}

	// The read-only mount is READABLE — it is read-only, not invisible.
	result, failure = execTool(t, srv, ws.WorkspaceId, "read", map[string]string{"file_path": "/repo/README.md"})
	if failure != "" {
		t.Fatalf("the read-only mount must still be readable: %s", failure)
	}
	if !strings.Contains(result, "# test") {
		t.Fatalf("read of /repo/README.md returned %q", truncate(result, 200))
	}
	_ = repo
}

// A symlink inside the workspace pointing out of it must not become a way
// out. The containment check has to run AFTER symlink resolution or it is
// checking a name rather than a location.
func TestASymlinkOutOfTheWorkspaceIsRefused(t *testing.T) {
	skipUntilInContainerToolServer(t)
	requireDocker(t)
	_, worktree := gitRepoWithDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, worktree, "")

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "escape")); err != nil {
		t.Fatal(err)
	}

	result, failure := execTool(t, srv, ws.WorkspaceId, "read",
		map[string]string{"file_path": "/work/escape/secret"})
	if failure == "" {
		t.Fatalf("a symlink out of the workspace was followed: %s", truncate(result, 200))
	}
	// It must be refused for the RIGHT reason. Before the fix this call also
	// failed — with "no such file or directory", because /work does not exist
	// on the host at all — which is a passing assertion for a workspace that
	// enforces nothing.
	if !strings.Contains(strings.ToLower(failure), "workspace") {
		t.Fatalf("refused, but not as an escape: %s", failure)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("…(+%d bytes)", len(s)-n)
}
