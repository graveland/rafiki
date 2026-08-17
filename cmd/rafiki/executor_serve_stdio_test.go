package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// stdioServer builds this binary, runs it in --serve-stdio mode over a plain
// subprocess pipe pair, and returns a Connect client speaking to it.
//
// No docker. The transport under test is the same one the container path uses —
// `docker exec -i` differs only in what sits between the pipes — so exercising
// it as a subprocess is both faster and a stricter isolation of the transport
// from the container plumbing.
func stdioServer(t *testing.T, root string) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "rafiki")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Child stderr goes to a file rather than a shared buffer: os/exec copies it
	// on its own goroutine, and a bytes.Buffer read from the test goroutine on
	// failure is a data race that -race will fail the suite for.
	errPath := filepath.Join(t.TempDir(), "stderr.log")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { errFile.Close() })

	childStderr := func() string {
		b, _ := os.ReadFile(errPath)
		return string(b)
	}

	cmd := exec.Command(bin, "executor", "serve-stdio", "--root", root)
	cmd.Stderr = errFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("child stderr:\n%s", childStderr())
		}
	})

	conn := execpool.NewStdioConn(stdout, stdin)
	httpClient, err := execpool.ClientForConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	return executorpbconnect.NewExecutorServiceClient(httpClient, "http://stdio")
}

// execTool drives one Execute call to completion, keeping the result and the
// failure message apart: a refusal is a Failed event, and folding the two
// together cannot tell a refusal from a success.
func execStdioTool(t *testing.T, client executorpbconnect.ExecutorServiceClient, tool string, input any) (result, failure string) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      tool,
		InputJson: raw,
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

// The mode exists so the tool server can run inside a container workspace,
// where workspace.Derive sets Network: "none" and there is therefore no socket
// to listen on. If this round trip works, the container path's only remaining
// unknown is docker itself.
func TestServeStdioAnswersDescribe(t *testing.T) {
	root := t.TempDir()
	client := stdioServer(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe over stdio: %v", err)
	}
	m := resp.Msg
	if len(m.Roots) != 1 || m.Roots[0] != root {
		t.Errorf("Roots = %v; want [%s] — --root must reach the server", m.Roots, root)
	}
	// The inner server is already inside the box. If it ever reported container
	// isolation it would try to provision containers of its own.
	if m.Isolation != "none" {
		t.Errorf("Isolation = %q; want none", m.Isolation)
	}
	if m.WorkspaceMode != "pinned" {
		t.Errorf("WorkspaceMode = %q; want pinned", m.WorkspaceMode)
	}
	if m.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Platform = %q; want %s/%s", m.Platform, runtime.GOOS, runtime.GOARCH)
	}
}

// The tools must actually operate on --root, not on the child's inherited
// working directory. That distinction is the entire point in the container: the
// outer executor passes the workspace workdir and nothing else may win.
func TestServeStdioRunsToolsAgainstRoot(t *testing.T) {
	root := t.TempDir()
	client := stdioServer(t, root)

	if _, failure := execStdioTool(t, client, "write", map[string]any{
		"file_path": filepath.Join(root, "hello.txt"),
		"content":   "hello over stdio\n",
	}); failure != "" {
		t.Fatalf("write: %s", failure)
	}

	// It landed on the real filesystem, at the path we asked for.
	b, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatalf("the write did not reach the filesystem: %v", err)
	}
	if !strings.Contains(string(b), "hello over stdio") {
		t.Fatalf("file contains %q", b)
	}

	result, failure := execStdioTool(t, client, "read", map[string]any{
		"file_path": filepath.Join(root, "hello.txt"),
	})
	if failure != "" {
		t.Fatalf("read: %s", failure)
	}
	if !strings.Contains(result, "hello over stdio") {
		t.Fatalf("read returned %q", result)
	}

	// A relative path resolves against --root. Inside the container this is what
	// makes an agent's "read main.go" mean the workspace's main.go.
	result, failure = execStdioTool(t, client, "read", map[string]any{"file_path": "hello.txt"})
	if failure != "" || !strings.Contains(result, "hello over stdio") {
		t.Fatalf("relative read: failure=%q result=%q", failure, result)
	}

	// bash runs there too, and its output survives the wire that its own stdout
	// shares with the HTTP/2 framing.
	result, failure = execStdioTool(t, client, "bash", map[string]any{"command": "pwd && cat hello.txt"})
	if failure != "" {
		t.Fatalf("bash: %s", failure)
	}
	if !strings.Contains(result, "hello over stdio") {
		t.Fatalf("bash output did not survive the stdio wire: %q", result)
	}
}

// `serve` still has two transports and must refuse an ambiguous pair. The
// stdio/socket/connect three-way ambiguity is gone by construction now that
// serve-stdio is its own subcommand — cobra cannot be asked for both.
func TestExecutorServeRejectsConflictingFlags(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rafiki")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"both transports", []string{"executor", "serve", "--socket", "/tmp/x.sock", "--connect", "127.0.0.1:9"},
			"mutually exclusive"},
		{"neither transport", []string{"executor", "serve"}, "is required"},
		{"container without an image", []string{"executor", "serve", "--socket", "/tmp/x.sock",
			"--isolation", "container"}, "requires --image"},
		{"ephemeral without isolation", []string{"executor", "serve", "--socket", "/tmp/x.sock",
			"--workspace-mode", "ephemeral"}, "does not support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(bin, tc.args...).CombinedOutput()
			if err == nil {
				t.Fatalf("expected a refusal, got success; output: %s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("refusal must explain itself; want %q in:\n%s", tc.want, out)
			}
		})
	}
}
