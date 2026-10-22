// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

func TestParseMCPCommandFlags(t *testing.T) {
	c := assert.NewAborting(t)
	got, err := executor.ParseMCPCommandFlags([]string{
		`hesabu=/Users/brent/go/bin/hesabu mcp --stdio`,
		`odd=/bin/echo a\ b \\ \"q"`,
	})
	c.Require().NoError(err, "ParseMCPCommandFlags")
	c.EqDeep(got["hesabu"], []string{"/Users/brent/go/bin/hesabu", "mcp", "--stdio"}, "hesabu argv")
	// A backslash escapes a space, a backslash and a quote — and nothing else
	// is a shell: no globbing, no expansion.
	c.EqDeep(got["odd"], []string{"/bin/echo", "a b", `\`, `"q"`}, "odd argv")
}

func TestParseMCPCommandFlagsRejects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no equals", "hesabu", "want name=argv"},
		{"empty name", "=hesabu mcp", "empty command name"},
		{"empty argv", "hesabu=", "empty argv"},
		{"only whitespace", "hesabu=   ", "empty argv"},
		{"trailing backslash", `hesabu=echo a\`, "trailing backslash"},
		{"duplicate", "", "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			args := []string{tc.in}
			if tc.name == "duplicate" {
				args = []string{"a=echo 1", "a=echo 2"}
			}
			_, err := executor.ParseMCPCommandFlags(args)
			c.Require().Error(err, "accepted %q", tc.in)
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}

// mcpFrames is everything one MCPStdio stream delivered, labelled by channel.
type mcpFrames struct {
	started *executorpb.MCPStdioStarted
	stdout  []byte
	stderr  []byte
	exit    *executorpb.MCPStdioExit
	err     error
}

// mcpCollect drains one MCPStdio stream to its end and returns the frames it
// saw plus the stream's terminal error (nil on a clean end).
func mcpCollect(t *testing.T, stream *connect.BidiStreamForClient[executorpb.MCPStdioRequest, executorpb.MCPStdioResponse]) mcpFrames {
	t.Helper()
	var f mcpFrames
	for {
		msg, err := stream.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				f.err = err
			}
			return f
		}
		switch {
		case msg.GetStarted() != nil:
			f.started = msg.GetStarted()
		case msg.GetStdout() != nil:
			f.stdout = append(f.stdout, msg.GetStdout().GetData()...)
		case msg.GetStderr() != nil:
			f.stderr = append(f.stderr, msg.GetStderr().GetData()...)
		case msg.GetExit() != nil:
			f.exit = msg.GetExit()
		}
	}
}

func TestMCPStdioRefusesUndeclaredName(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root:        t.TempDir(),
		MCPCommands: map[string][]string{"hesabu": {"echo", "hi"}},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "nope"}},
	}), "send start")

	f := mcpCollect(t, stream)
	c.Require().Error(f.err, "undeclared name was accepted")
	// The message names what this executor DOES declare, so an operator
	// reading a child's log can see the typo without opening the unit file.
	c.StrContains(f.err.Error(), `undeclared mcp command "nope"`, "error")
	c.StrContains(f.err.Error(), "hesabu", "error names the declared names")
	c.False(f.started != nil, "a process was started for an undeclared name")
}

func TestMCPStdioSpawnsWithOperatorArgvAndExecutorEnv(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(),
		// The argv and the env are the EXECUTOR's; the request carried only a
		// name. stderr is written first, then stdout after a pause, so the
		// labelled channels are distinguishable even if a stderr frame is
		// dropped under a stalled send (which is allowed by design).
		MCPCommands: map[string][]string{
			"fixture": {"sh", "-c", `echo "env=$MCP_TEST_VALUE" >&2; sleep 0.2; echo hello`},
		},
		Env: []string{"MCP_TEST_VALUE=opaque-value", "PATH=/usr/bin:/bin"},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "fixture"}},
	}), "send start")
	c.NoError(stream.CloseRequest(), "half-close")

	f := mcpCollect(t, stream)
	c.NoError(f.err, "stream")
	c.Require().True(f.started != nil, "started frame missing")
	c.True(f.started.GetPid() > 0, "pid = %d, want > 0", f.started.GetPid())
	c.Eq(f.started.GetProgram(), "sh", "program")
	c.StrContains(string(f.stdout), "hello", "stdout")
	c.StrContains(string(f.stderr), "env=opaque-value", "stderr")
	c.Require().True(f.exit != nil, "exit frame missing")
	c.Eq(f.exit.GetCode(), int32(0), "exit code")
}

func TestMCPStdioExitCarriesStderrForAClientThatMissedFrames(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root:        t.TempDir(),
		MCPCommands: map[string][]string{"fixture": {"sh", "-c", `echo "fatal: no credentials" >&2; exit 3`}},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "fixture"}},
	}), "send start")

	f := mcpCollect(t, stream)
	c.NoError(f.err, "stream")
	c.Require().True(f.exit != nil, "exit frame missing")
	c.Eq(f.exit.GetCode(), int32(3), "exit code")
	c.StrContains(f.exit.GetStderrTail(), "fatal: no credentials", "stderr_tail")
}

func TestMCPStdioSecondStartIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root:        t.TempDir(),
		MCPCommands: map[string][]string{"fixture": {"sh", "-c", "sleep 5"}},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	start := &executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "fixture"}},
	}
	c.NoError(stream.Send(start), "send start")
	c.NoError(stream.Send(start), "send second start")

	f := mcpCollect(t, stream)
	c.Require().Error(f.err, "a second start was accepted")
	c.StrContains(f.err.Error(), "more than once", "error")
}

func TestMCPStdioFirstFrameMustBeStart(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir()})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Stdin{Stdin: []byte("{}\n")},
	}), "send stdin")

	f := mcpCollect(t, stream)
	c.Require().Error(f.err, "a stdin-first stream was accepted")
	c.StrContains(f.err.Error(), "first message must be a start", "error")
}

func TestMCPStdioStreamCloseKillsTheProcessGroup(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	gcFile := filepath.Join(root, "grandchild.pid")
	srv := executor.NewServer(executor.Options{
		Root: root,
		// A forked grandchild: sleep is a CHILD of sh, so only a process-group
		// kill reaches it. A plain Process.Kill would leave it running. It
		// records its pid so the test can ask about that exact process — a
		// `kill(-pgid, 0)` probe is unreliable here, since a reparented
		// process is not this test's to reap.
		MCPCommands: map[string][]string{
			"fixture": {"sh", "-c", `sleep 300 & echo $! > ` + gcFile + `; wait`},
		},
	})
	client, httpSrv := newTestClientWithServer(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "fixture"}},
	}), "send start")

	msg, err := stream.Receive()
	c.Require().NoError(err, "receive started")
	c.Require().True(msg.GetStarted() != nil, "first frame was not started")
	pid := int(msg.GetStarted().GetPid())

	gcPID := waitForPIDFile(t, gcFile)
	c.Require().True(pid > 0 && gcPID > 0, "pids: child %d, grandchild %d", pid, gcPID)
	c.NoError(syscall.Kill(gcPID, 0), "grandchild %d was not running before the close", gcPID)

	// The stream ending is what a daemon disconnect, a killed child and a
	// rolling pod all look like: the handler's context is cancelled and the
	// process group must go with it.
	c.NoError(httpSrv.Close(), "close the server")

	deadline := time.Now().Add(10 * time.Second)
	for {
		if processDead(gcPID) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(gcPID, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the stream closing", gcPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// newTestClientWithServer is newTestClient, handing back the http.Server so a
// test can end the transport (and with it every handler context) on purpose.
func newTestClientWithServer(t *testing.T, srv *executor.Server) (executorpbconnect.ExecutorServiceClient, *http.Server) {
	t.Helper()
	c := assert.NewAborting(t)
	sockPath := filepath.Join("/tmp", "rafiki-mcp-"+strings.ReplaceAll(t.Name(), "/", "_")+".sock")
	os.Remove(sockPath)
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
		&http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		}},
		"http://executor",
	)
	return client, httpSrv
}

// processDead reports whether pid has stopped doing work. ESRCH is the clean
// answer; a zombie counts too, because a reparented process is not this test's
// to reap and its container's init may not have got to it — "killed and
// awaiting reap" is dead for every purpose this feature has.
func processDead(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] == 'Z' || s[i+2] == 'X'
}

// waitForPIDFile polls for a pid file the fixture writes, returning its value.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && n > 0 {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture never wrote %s", path)
	return 0
}

func TestMCPStdioBudgetRefusesTheFourthQuickFailure(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root:        t.TempDir(),
		MCPCommands: map[string][]string{"flapper": {"sh", "-c", `echo "boom: config missing" >&2; exit 1`}},
	})
	client := newTestClient(t, srv)

	dial := func() mcpFrames {
		stream := client.MCPStdio(context.Background())
		c.NoError(stream.Send(&executorpb.MCPStdioRequest{
			Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "flapper"}},
		}), "send start")
		return mcpCollect(t, stream)
	}
	for i := 0; i < 3; i++ {
		f := dial()
		c.Require().NoError(f.err, "spawn %d", i+1)
		c.Require().True(f.exit != nil, "spawn %d produced no exit frame", i+1)
	}
	// The fourth is REFUSED before spawning, and carries the last stderr tail
	// so the operator sees why without digging through the executor's log.
	f := dial()
	c.Require().Error(f.err, "a crash-looping server was spawned a fourth time")
	c.False(f.started != nil, "a process was started despite the cool-off")
	c.StrContains(f.err.Error(), "refusing new spawns", "error")
	// The count has to be the number of failures that tripped it, not the
	// length of a record the trip just cleared.
	c.StrContains(f.err.Error(), "after 3 failures", "error")
	c.StrContains(f.err.Error(), "boom: config missing", "error carries the last stderr tail")
}

// A dropped stdout frame would silently truncate the protocol stream, so the
// relay must hand back every byte the process wrote, in order.
func TestMCPStdioStdoutIsByteExact(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root:        t.TempDir(),
		MCPCommands: map[string][]string{"fixture": {"sh", "-c", `i=0; while [ $i -lt 200 ]; do printf '{"jsonrpc":"2.0","id":%d}\n' $i; i=$((i+1)); done`}},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "fixture"}},
	}), "send start")

	f := mcpCollect(t, stream)
	c.NoError(f.err, "stream")
	var want bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&want, "{\"jsonrpc\":\"2.0\",\"id\":%d}\n", i)
	}
	c.Eq(string(f.stdout), want.String(), "stdout")
}

// Describe self-reports the declared names, sorted: it is what the daemon
// resolves a via_executor entry against, and it only ever NARROWS what this
// executor will host.
func TestDescribeAdvertisesMCPCommands(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(),
		MCPCommands: map[string][]string{
			"hesabu":   {"hesabu", "mcp", "--stdio"},
			"codescan": {"codescan"},
		},
	})
	client := newTestClient(t, srv)
	resp, err := client.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	c.NoError(err, "Describe")
	c.EqDeep(resp.Msg.GetMcpCommands(), []string{"codescan", "hesabu"}, "mcp_commands")
}

// An executor that declares none advertises none — the default, and the right
// one.
func TestDescribeAdvertisesNoMCPCommandsByDefault(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir()})
	client := newTestClient(t, srv)
	resp, err := client.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	c.NoError(err, "Describe")
	c.Eq(len(resp.Msg.GetMcpCommands()), 0, "mcp_commands: %v", resp.Msg.GetMcpCommands())
}

// A forked child must not outlive the stream when the LEADER exits on its own:
// D6 says a process exists only for the life of its stream, and a server that
// spawns a helper (npx, a shell wrapper) is exactly the case a bare wait misses.
func TestMCPStdioLeaderExitSweepsItsGroup(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	gcFile := filepath.Join(root, "grandchild.pid")
	srv := executor.NewServer(executor.Options{
		Root: root,
		// The leader exits immediately AFTER forking, so its own death is what
		// ends the stream; only a group sweep reaches the sleep.
		MCPCommands: map[string][]string{
			"spotless": {"sh", "-c", `sleep 300 & echo $! > ` + gcFile + `; exit 0`},
		},
	})
	client := newTestClient(t, srv)

	stream := client.MCPStdio(context.Background())
	c.NoError(stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: "spotless"}},
	}), "send start")

	f := mcpCollect(t, stream)
	c.NoError(f.err, "stream")
	c.Require().True(f.exit != nil, "exit frame missing")
	c.Eq(f.exit.GetCode(), int32(0), "exit code")

	gcPID := waitForPIDFile(t, gcFile)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if processDead(gcPID) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(gcPID, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the leader that forked it", gcPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
