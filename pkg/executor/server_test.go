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
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

func TestDescribeReportsCapabilities(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	resp, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	c.Require().NoError(err, "Describe")
	m := resp.Msg
	c.NotEq("", m.ExecutorId, "ExecutorId must be set")
	c.Eq(runtime.GOOS+"/"+runtime.GOARCH, m.Platform, "Platform = %q; want %s/%s", m.Platform, runtime.GOOS, runtime.GOARCH)
	want := []string{"read", "write", "edit", "glob", "grep", "ls", "bash"}
	got := map[string]bool{}
	for _, tool := range m.Tools {
		got[tool] = true
	}
	for _, w := range want {
		c.False(!got[w], "Describe omits tool %q — the parent uses this list to decide what to route", w)
	}
	// Parent-side tools are RPCs the daemon implements itself; the executor's
	// registry does not contain them, so Describe must not claim it does.
	for _, p := range []string{"bash_start", "bash_output", "bash_kill", "task_add", "web_search"} {
		c.False(got[p], "Describe claims parent-side tool %q, which this registry does not serve", p)
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
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	resp, err := client.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
	c.Require().NoError(err, "Health")
	c.False(resp.Msg.Draining, "a fresh executor must not report draining")
	c.Empty(resp.Msg.RunningHandles, "a fresh executor has no running handles")
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
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	for _, tool := range []string{"task_add", "task_list", "web_search", "web_fetch", "skill"} {
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "x", Tool: tool, InputJson: []byte(`{}`), Timeout: durationpb.New(5 * time.Second),
		}))
		c.NoError(err, "%s: transport error, want a typed Failure", tool)
		var failed bool
		for stream.Receive() {
			if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
				failed = true
			}
		}
		c.NoError(stream.Err(), "%s: stream error, want a typed Failure", tool)
		c.True(failed, "%s: executor served a parent-side tool", tool)
	}
}

func TestExecuteHonoursTimeoutMs(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)
	ctx := context.Background()

	stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		CallId: "slow", Tool: "bash", InputJson: []byte(`{"command":"sleep 10"}`), Timeout: durationpb.New(500 * time.Millisecond),
	}))
	c.NoError(err)
	var code executorpb.Failure_Code
	for stream.Receive() {
		if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			code = ev.Failed.Code
		}
	}
	_ = stream.Err()
	c.Eq(executorpb.Failure_CODE_TIMEOUT, code, "failure code")
}

// newTestClient starts a server on a temp unix socket and returns a Connect
// client dialing it. The caller is responsible for cleanup via t.Cleanup.
func newTestClient(t *testing.T, srv *executor.Server) executorpbconnect.ExecutorServiceClient {
	t.Helper()
	c := assert.NewAborting(t)

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
	c.NoError(err, "listen")
	c.NoError(os.Chmod(sockPath, 0o600), "chmod")
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
