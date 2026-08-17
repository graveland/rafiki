package executor_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		Image:         requireWorkspaceImage(t),
		Version:       "test",
	})
}

// testWorkspaceImage is built from the repo's own Dockerfile rather than pulled.
const testWorkspaceImage = "rafiki-workspace:test"

var (
	workspaceImageOnce sync.Once
	workspaceImageErr  error
)

// requireWorkspaceImage builds the reference workspace image once per test
// binary and returns its tag.
//
// These tests used to run against alpine:3.19, which has neither ripgrep nor an
// executor binary — so they could only ever exercise the paths that do not need
// the image to be right. The image is part of this repo's code now (Dockerfile,
// `--target workspace`), so it is built like TestMain builds the daemon rather
// than assumed present: a loud skip would let the container path stop being
// tested the moment someone had not built it by hand, and building also keeps
// the baked binary in step with the source under test, which is what makes the
// version-skew warning meaningful.
func requireWorkspaceImage(t *testing.T) string {
	t.Helper()
	workspaceImageOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			workspaceImageErr = err
			return
		}
		// Cached after the first run in a working tree; a cold build is ~1 minute.
		cmd := exec.Command("docker", "build", "--target", "workspace", "-t", testWorkspaceImage, root)
		if out, err := cmd.CombinedOutput(); err != nil {
			workspaceImageErr = fmt.Errorf("docker build --target workspace: %v\n%s", err, out)
		}
	})
	if workspaceImageErr != nil {
		// Fail rather than skip: docker is available (requireDocker ran), so a
		// build failure is a real break in the image or the Dockerfile.
		t.Fatalf("cannot build the workspace image: %v", workspaceImageErr)
	}
	return testWorkspaceImage
}

// moduleRoot walks up from the working directory to the directory holding go.mod,
// which is the docker build context.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
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

// The image contract has to be enforced at Provision, because every way of
// discovering it later is worse. ripgrep is the sharp case: glob and grep
// DECLINE when rg is absent rather than erroring, so an agent on a bad image is
// told "no matches" and has nothing useful to report. An image with no executor
// binary is worse still — every tool call fails with a transport error naming
// nothing.
//
// alpine:3.19 is the fixture precisely because it has neither. It is what these
// tests used to run against.
func TestProvisionRefusesAnImageMissingTheContract(t *testing.T) {
	requireDocker(t)
	srv := executor.NewServer(executor.Options{
		Root:          t.TempDir(),
		Isolation:     "container",
		WorkspaceMode: "ephemeral",
		Image:         "alpine:3.19",
		Version:       "test",
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	desc, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId:       "test-child",
		WorkspaceMode: "ephemeral",
		Mounts:        []*executorpb.Mount{{HostPath: t.TempDir(), ContainerPath: "/work"}},
		Workdir:       "/work",
		Network:       "none",
	}))
	if err == nil {
		t.Fatal("Provision accepted an image with no executor binary and no ripgrep")
	}

	// The refusal has to be actionable: which image, what is missing, and how to
	// get a good one. An accurate message nobody can act on costs a debugging
	// session.
	for _, want := range []string{"alpine:3.19", "rafiki", "--target workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q; got: %v", want, err)
		}
	}

	// And it must not leak the container it started before validating. A stray
	// `sleep infinity` holds its mounts for as long as the docker daemon lives.
	out, _ := exec.Command("docker", "ps", "-a", "-q",
		"--filter", "label=dev.graveland.rafiki.executor="+desc.Msg.ExecutorId).Output()
	if len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("a failed Provision leaked its container: %s", bytes.TrimSpace(out))
	}
}

// Provision must not return until a tool server is running INSIDE the container
// and has answered Describe. Returning a dead one turns into a confusing failure
// on the child's first tool call, somewhere else entirely, rather than a clear
// one here.
//
// `docker top` lists processes as the CONTAINER sees them, which is what makes
// this test meaningful: a tool server running on the host — the D1 arrangement —
// would not appear.
func TestProvisionStartsAToolServerInsideTheContainer(t *testing.T) {
	requireDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, t.TempDir(), "")

	out, err := exec.Command("docker", "top", ws.WorkspaceId).CombinedOutput()
	if err != nil {
		t.Fatalf("docker top: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "serve-stdio") {
		t.Fatalf("no tool server is running inside the container; docker top said:\n%s", out)
	}
}

// Release must stop the `docker exec` process too, not just the container.
// The exec client lives on the HOST — it is the process whose stdio carries the
// wire — so removing the container without it leaves an orphan holding a pipe
// pair for as long as the executor lives.
func TestReleaseStopsTheInnerServerProcess(t *testing.T) {
	requireDocker(t)
	srv := newContainerExecutor(t)
	ws := provision(t, srv, t.TempDir(), "")
	client := newTestClient(t, srv)
	ctx := context.Background()

	if n := hostExecProcesses(t, ws.WorkspaceId); n == 0 {
		t.Fatal("no `docker exec` process on the host for this workspace; the test cannot detect an orphan")
	}

	if _, err := client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
		WorkspaceId: ws.WorkspaceId,
	})); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Kill is asynchronous in the sense that the process table takes a moment to
	// reflect a reaped child; poll briefly rather than racing it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hostExecProcesses(t, ws.WorkspaceId) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a `docker exec` process for %s outlived Release", ws.WorkspaceId)
}

// hostExecProcesses counts host processes whose command line names this
// workspace — the `docker exec -i <id> rafiki executor serve-stdio` client.
func hostExecProcesses(t *testing.T, workspaceID string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "docker exec.*"+workspaceID).Output()
	if err != nil {
		// pgrep exits 1 with no output when nothing matches, which is not an error.
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
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

	name := containerNameFor(ws.WorkspaceId)
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+name, "-q").Output()
	if len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("container %s survived Release", name)
	}
}

// containerNameFor derives the docker container name from the workspace id.
//
// The workspace id IS the container name — containerBackend.Provision builds it
// as "rafiki-ws-" + randomID() and passes it straight to `docker run --name`.
// This helper used to prepend the prefix a second time, producing
// "rafiki-ws-rafiki-ws-…", which matches no container: the `docker ps` filter
// below was therefore always empty and TestReleaseRemovesTheContainer could not
// fail. A test that cannot fail is worse than no test, because it is counted.
func containerNameFor(workspaceID string) string {
	return workspaceID
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
