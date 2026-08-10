package executor_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

func TestDescribeReportsCapabilities(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Concurrency: 6, Version: "test"})
	client := testClient(t, srv)
	ctx := context.Background()

	resp, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	m := resp.Msg
	if m.ExecutorId == "" {
		t.Error("ExecutorId must be set")
	}
	if m.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Platform = %q; want %s/%s", m.Platform, runtime.GOOS, runtime.GOARCH)
	}
	want := []string{"read", "write", "edit", "glob", "grep", "bash",
		"bash_start", "bash_output", "bash_kill"}
	got := map[string]bool{}
	for _, tool := range m.Tools {
		got[tool] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe omits tool %q — the parent uses this list to decide what to route", w)
		}
	}
	if len(m.Roots) != 1 || m.Roots[0] != root {
		t.Errorf("Roots = %v; want [%s]", m.Roots, root)
	}
	if m.Isolation != "none" {
		t.Errorf("Isolation = %q; want none", m.Isolation)
	}
	if m.WorkspaceMode != "pinned" {
		t.Errorf("WorkspaceMode = %q; want pinned", m.WorkspaceMode)
	}
}

func TestHealthReportsNoRunningHandles(t *testing.T) {
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := testClient(t, srv)
	ctx := context.Background()

	resp, err := client.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Msg.Draining {
		t.Error("a fresh executor must not report draining")
	}
	if len(resp.Msg.RunningHandles) != 0 {
		t.Error("a fresh executor has no running handles")
	}
}

func TestConformance(t *testing.T) {
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Concurrency: 6, Version: "test"})
	client := testClient(t, srv)
	RunConformance(t, client, root)
}

// testClient starts a server on a temp unix socket and returns a Connect
// client dialing it. The caller is responsible for cleanup via t.Cleanup.
func testClient(t *testing.T, handler executorpbconnect.ExecutorServiceHandler) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	sockPath := filepath.Join("/tmp", "rafiki-exec-"+t.Name()+".sock")
	os.Remove(sockPath) // stale from a crashed run
	t.Cleanup(func() { os.Remove(sockPath) })

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(handler))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protos}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	client := executorpbconnect.NewExecutorServiceClient(
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
	return client
}
