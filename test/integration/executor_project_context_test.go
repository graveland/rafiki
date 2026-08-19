package integration_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// TestExecutorProjectContext drives the ProjectContext RPC over a real unix
// socket: an executor serving a root containing a CLAUDE.md answers with that
// file, includes expanded, and refuses an unknown workspace. This is the wire
// half of "project instruction files come from the workspace's machine" — the
// daemon-side composition is unit-tested in pkg/fundi and cmd/rafikid.
func TestExecutorProjectContext(t *testing.T) {
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "rafiki-pctx")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := os.WriteFile(filepath.Join(dir, "extra.md"), []byte("INCLUDED_E2E_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("EXECUTOR_E2E_MARKER\n@extra.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(dir, "executor.sock")
	cmd := exec.Command(cliPath, "executor", "serve",
		"--socket", sock,
		"--root", dir,
		"--no-lsp",
	)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start executor: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	// Wait for the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("executor never listened on %s", sock)
		}
		time.Sleep(20 * time.Millisecond)
	}

	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	client := executorpbconnect.NewExecutorServiceClient(&http.Client{Transport: transport}, "http://executor")

	ctx := context.Background()
	prov, err := client.Provision(ctx, connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_e2e"}))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	pc, err := client.ProjectContext(ctx, connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: prov.Msg.WorkspaceId}))
	if err != nil {
		t.Fatalf("ProjectContext: %v", err)
	}
	if !strings.Contains(pc.Msg.ContextFiles, "EXECUTOR_E2E_MARKER") {
		t.Errorf("ContextFiles = %q, want the workspace's CLAUDE.md", pc.Msg.ContextFiles)
	}
	if !strings.Contains(pc.Msg.ContextFiles, "INCLUDED_E2E_MARKER") {
		t.Errorf("ContextFiles = %q, want the @include expanded", pc.Msg.ContextFiles)
	}
	if strings.Contains(pc.Msg.ContextFiles, "@extra.md") {
		t.Errorf("include line was not inlined: %q", pc.Msg.ContextFiles)
	}

	_, err = client.ProjectContext(ctx, connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: "ws_nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown workspace: code %v, want NotFound", connect.CodeOf(err))
	}
}
