package executor_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// requireDocker skips LOUDLY. CLAUDE.md records why this matters: the local
// dev wrapper compacts test output and swallows SKIP lines entirely, so a
// suite that skipped everything looks identical to one that passed. Writing to
// stderr survives that; t.Skip alone does not.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("RAFIKI_TEST_DOCKER") == "0" {
		t.Skip("RAFIKI_TEST_DOCKER=0")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintf(os.Stderr,
			"\n!!! SKIPPING %s: docker not on PATH. Container isolation is UNVERIFIED in this run.\n\n",
			t.Name())
		t.Skip("docker not available")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr,
			"\n!!! SKIPPING %s: docker present but not running (%v). Container isolation is UNVERIFIED.\n%s\n\n",
			t.Name(), err, out)
		t.Skip("docker daemon not running")
	}
}

func newContainerExecutor(t *testing.T) *executor.Server {
	t.Helper()
	return executor.NewServer(executor.Options{
		Root:          t.TempDir(),
		Isolation:     "container",
		WorkspaceMode: "ephemeral",
		Image:         "alpine:3.19",
		Version:       "test",
	})
}

func provision(t *testing.T, srv *executor.Server, worktree, repo string) *executorpb.ProvisionResponse {
	t.Helper()
	client := newTestClient(t, srv)
	ctx := context.Background()

	mounts := []*executorpb.Mount{
		{HostPath: worktree, ContainerPath: "/work", ReadOnly: false},
	}
	if repo != "" {
		mounts = append(mounts, &executorpb.Mount{HostPath: repo, ContainerPath: "/repo", ReadOnly: true})
	}

	resp, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId:       "test-child",
		WorkspaceMode: "ephemeral",
		Mounts:        mounts,
		Workdir:       "/work",
		Network:       "none",
	}))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
			WorkspaceId: resp.Msg.WorkspaceId,
		}))
	})
	return resp.Msg
}

func execBash(t *testing.T, srv *executor.Server, ws *executorpb.ProvisionResponse, command string) (string, error) {
	t.Helper()
	client := newTestClient(t, srv)
	ctx := context.Background()

	input := fmt.Sprintf(`{"command":%q}`, command)
	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "bash",
		InputJson:   []byte(input),
		WorkspaceId: ws.WorkspaceId,
	}))
	if err != nil {
		return "", err
	}
	var resultText string
	var failed string
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			failed = ev.Failed.Message
		}
	}
	if e := stream.Err(); e != nil {
		return "", e
	}
	if failed != "" {
		return resultText, fmt.Errorf("failed: %s", failed)
	}
	return resultText, nil
}

// The grant is the mount. A read-only mount must be enforced by the KERNEL,
// not by a tool refusing politely — that is the entire argument for containers
// over userspace path checks.
func TestReadOnlyMountIsEnforcedAgainstBash(t *testing.T) {
	requireDocker(t)
	repo, worktree := gitRepoWithDocker(t)
	srv := newContainerExecutor(t)

	ws := provision(t, srv, worktree, repo)

	// Writing under /work succeeds.
	out, err := execBash(t, srv, ws, "touch /work/ok && echo yes")
	if err != nil || !strings.Contains(out, "yes") {
		t.Fatalf("the worktree must be writable: %v %s", err, out)
	}
	// Writing under /repo does not — and specifically not through bash, which
	// is the case a userspace path check cannot cover.
	out, err = execBash(t, srv, ws, "touch /repo/nope 2>&1; echo rc=$?")
	if !strings.Contains(out, "Read-only file system") && !strings.Contains(out, "rc=1") {
		t.Fatalf("/repo must be read-only to the shell, got: %s (err=%v)", out, err)
	}
}

// Nothing outside the mounts is reachable. This is the property that makes
// "twenty workers fanning out on cheap models" something you can leave running.
func TestHostFilesystemIsNotReachable(t *testing.T) {
	requireDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, t.TempDir(), "")

	out, _ := execBash(t, srv, ws, "ls ~/.ssh 2>&1; echo rc=$?")
	if !strings.Contains(out, "rc=") || strings.Contains(out, "id_rsa") {
		t.Fatalf("the host home directory leaked into the container: %s", out)
	}
}

func TestNetworkNoneHasNoEgress(t *testing.T) {
	requireDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, t.TempDir(), "")

	out, _ := execBash(t, srv, ws, "getent hosts example.com >/dev/null 2>&1; echo rc=$?")
	if strings.Contains(out, "rc=0") {
		t.Fatal("network=none must have no DNS or egress")
	}
}

// Release actually removes the container. A leaked container per child is a
// slow-motion outage on a box running unattended workers.
func TestReleaseRemovesTheContainer(t *testing.T) {
	requireDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, t.TempDir(), "")

	client := newTestClient(t, srv)
	ctx := context.Background()
	_, err := client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
		WorkspaceId: ws.WorkspaceId,
	}))
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	// Fetch the workspace id to derive the container name.
	name := containerNameFor(ws.WorkspaceId)
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+name, "-q").Output()
	if len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("container %s survived Release", name)
	}
}

// containerNameFor derives the docker container name from the workspace id.
// Must match the naming scheme in the container backend.
func containerNameFor(workspaceID string) string {
	return "rafiki-ws-" + workspaceID
}

func gitRepoWithDocker(t *testing.T) (repo, worktree string) {
	t.Helper()
	repo = t.TempDir()
	runDocker(t, repo, "git", "init", "-b", "main")
	runDocker(t, repo, "git", "config", "user.email", "test@test")
	runDocker(t, repo, "git", "config", "user.name", "Test")
	if err := os.WriteFile(repo+"/README.md", []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runDocker(t, repo, "git", "add", "README.md")
	runDocker(t, repo, "git", "commit", "-m", "init")

	worktree = t.TempDir() + "/wt"
	runDocker(t, repo, "git", "worktree", "add", worktree)
	return
}

func runDocker(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", argv, out)
	}
}
