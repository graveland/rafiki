package integration_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// requireDocker skips LOUDLY so a silent skip is not mistaken for a pass.
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

// TestIntegration_ContainerIsolation verifies the full container executor path:
// provision, execute, read-only enforcement, host isolation, release.
func TestIntegration_ContainerIsolation(t *testing.T) {
	requireDocker(t)

	worktree := t.TempDir()
	repo := t.TempDir()

	// Create a git repo for the repo mount.
	run(t, repo, "git", "init", "-b", "main")
	run(t, repo, "git", "config", "user.email", "test@test")
	run(t, repo, "git", "config", "user.name", "Test")
	if err := os.WriteFile(repo+"/README.md", []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "commit", "-m", "init")

	srv := executor.NewServer(executor.Options{
		Root:          worktree,
		Isolation:     "container",
		WorkspaceMode: "ephemeral",
		Image:         "alpine:3.19",
		Version:       "test",
	})

	client := startExecutorServer(t, srv)
	ctx := context.Background()

	// Provision with worktree rw and repo ro.
	resp, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{
		ChildId:       "int-child",
		WorkspaceMode: "ephemeral",
		Mounts: []*executorpb.Mount{
			{HostPath: worktree, ContainerPath: "/work", ReadOnly: false},
			{HostPath: repo, ContainerPath: "/repo", ReadOnly: true},
		},
		Workdir: "/work",
		Network: "none",
	}))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	wsID := resp.Msg.WorkspaceId
	t.Cleanup(func() {
		_, _ = client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
			WorkspaceId: wsID,
		}))
	})

	t.Run("worktree writable", func(t *testing.T) {
		out := execBash(t, client, wsID, "touch /work/ok && echo yes")
		if !strings.Contains(out, "yes") {
			t.Fatalf("worktree must be writable: %s", out)
		}
	})

	t.Run("repo read-only", func(t *testing.T) {
		out := execBash(t, client, wsID, "touch /repo/nope 2>&1; echo rc=$?")
		if !strings.Contains(out, "Read-only") && !strings.Contains(out, "rc=1") {
			t.Fatalf("repo must be read-only: %s", out)
		}
	})

	t.Run("host unreachable", func(t *testing.T) {
		out := execBash(t, client, wsID, "ls ~/.ssh 2>&1; echo rc=$?")
		if strings.Contains(out, "id_rsa") {
			t.Fatalf("host home directory leaked into container: %s", out)
		}
	})

	t.Run("no network egress", func(t *testing.T) {
		out := execBash(t, client, wsID, "getent hosts example.com >/dev/null 2>&1; echo rc=$?")
		if strings.Contains(out, "rc=0") {
			t.Fatal("network=none must have no DNS or egress")
		}
	})
}

func execBash(t *testing.T, client executorpbconnect.ExecutorServiceClient, wsID string, command string) string {
	t.Helper()
	ctx := context.Background()

	input := fmt.Sprintf(`{"command":%q}`, command)
	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "bash",
		InputJson:   []byte(input),
		WorkspaceId: wsID,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resultText string
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			resultText = ev.Failed.Message
		}
	}
	_ = stream.Err()
	return resultText
}

func run(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", argv, out)
	}
}

// startExecutorServer starts an executor server on a temp socket and returns a client.
func startExecutorServer(t *testing.T, srv *executor.Server) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	sockPath := "/tmp/rafiki-int-" + t.Name() + ".sock"
	os.Remove(sockPath)
	t.Cleanup(func() { os.Remove(sockPath) })

	return newExecutorClient(t, sockPath, srv)
}

func newExecutorClient(t *testing.T, sockPath string, srv *executor.Server) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { httpSrv.Close() })
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Logf("serve: %v", err)
		}
	}()

	return executorpbconnect.NewExecutorServiceClient(
		&http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
		"http://executor",
	)
}