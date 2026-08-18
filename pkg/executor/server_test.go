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
	"strings"
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
	client := newTestClient(t, srv)
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
	// Isolation and WorkspaceMode are deliberately EMPTY here. They are
	// self-reported fields, and the authoritative copies live on the executor's
	// database row. An executor that filled them in would be asserting facts
	// that gate its own placement.
	if m.Isolation != "" || m.WorkspaceMode != "" {
		t.Errorf("Describe must not self-report isolation/workspace_mode; got %q/%q",
			m.Isolation, m.WorkspaceMode)
	}
}

func TestHealthReportsNoRunningHandles(t *testing.T) {
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
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
	client := newTestClient(t, srv)
	RunConformance(t, client, root)
}

// The executor must not carry the parent's credentialed tools. A registry
// built from the full blueprint also has a nil task store, so an Execute
// naming task_add nil-derefs and panics the handler.
func TestExecutorDoesNotServeParentSideTools(t *testing.T) {
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	for _, tool := range []string{"task_add", "task_list", "web_search", "web_fetch", "skill"} {
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "x", Tool: tool, InputJson: []byte(`{}`), TimeoutMs: 5000,
		}))
		if err != nil {
			t.Fatalf("%s: transport error, want a typed Failure: %v", tool, err)
		}
		var failed bool
		for stream.Receive() {
			if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
				failed = true
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("%s: stream error, want a typed Failure: %v", tool, err)
		}
		if !failed {
			t.Fatalf("%s: executor served a parent-side tool", tool)
		}
	}
}

func TestExecuteHonoursTimeoutMs(t *testing.T) {
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		CallId: "slow", Tool: "bash", InputJson: []byte(`{"command":"sleep 10"}`), TimeoutMs: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var code executorpb.Failure_Code
	for stream.Receive() {
		if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			code = ev.Failed.Code
		}
	}
	_ = stream.Err()
	if code != executorpb.Failure_CODE_TIMEOUT {
		t.Fatalf("failure code = %v, want CODE_TIMEOUT", code)
	}
}

// newTestClient starts a server on a temp unix socket and returns a Connect
// client dialing it. The caller is responsible for cleanup via t.Cleanup.
func newTestClient(t *testing.T, srv *executor.Server) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	// t.Name() carries "/" for every subtest, which turns the socket path into
	// a directory that does not exist; the failure ("bind: no such file or
	// directory") reads as a permissions problem rather than as this.
	sockPath := filepath.Join("/tmp", "rafiki-exec-"+strings.ReplaceAll(t.Name(), "/", "_")+".sock")
	os.Remove(sockPath) // stale from a crashed run
	t.Cleanup(func() { os.Remove(sockPath) })

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
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
