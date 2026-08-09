package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

func TestManager_For_NoMatch(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Extensions: []string{".go"}},
		},
	}, "/tmp")

	ctx := context.Background()
	client, err := mgr.For(ctx, "/tmp/foo.py")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if client != nil {
		t.Error("expected nil client for unmatched extension")
	}
}

func TestManager_For_Match(t *testing.T) {
	dir := t.TempDir()

	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found: %v", err)
	}

	// Create a minimal Go module.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: goplsPath, Extensions: []string{".go"}},
		},
	}, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mgr.For(ctx, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client for .go file")
	}

	// Second call for same language should return the same client.
	client2, err := mgr.For(ctx, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("For (second): %v", err)
	}
	if client != client2 {
		t.Error("expected same client on second For call")
	}

	// Shutdown.
	mgr.Shutdown(ctx)
}

func TestManager_ServerFor(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go":     {Command: "gopls", Extensions: []string{".go"}},
			"python": {Command: "pyright", Extensions: []string{".py"}},
		},
	}, "/tmp")

	tests := []struct {
		path   string
		want   string
		wantOk bool
	}{
		{"/tmp/main.go", "go", true},
		{"/tmp/thing.py", "python", true},
		{"/tmp/readme.md", "", false},
		{"/tmp/nosuffix", "", false},
	}

	for _, tt := range tests {
		name, _, ok := mgr.serverFor(tt.path)
		if ok != tt.wantOk {
			t.Errorf("serverFor(%q): ok=%v, want %v", tt.path, ok, tt.wantOk)
		}
		if name != tt.want {
			t.Errorf("serverFor(%q): name=%q, want %q", tt.path, name, tt.want)
		}
	}
}

func TestManager_NotifyChange(t *testing.T) {
	dir := t.TempDir()

	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: goplsPath, Extensions: []string{".go"}},
		},
	}, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// NotifyChange on a path without first calling For should lazily start.
	if err := mgr.NotifyChange(ctx, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("NotifyChange: %v", err)
	}

	mgr.Shutdown(ctx)
}

func TestManager_Shutdown(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Extensions: []string{".go"}},
		},
	}, "/tmp")
	ctx := context.Background()
	mgr.Shutdown(ctx)

	// Operations after shutdown should fail.
	_, err := mgr.For(ctx, "/tmp/main.go")
	if err == nil {
		t.Error("expected error after shutdown")
	}
}

// ---- fake LSP server subprocess (Finding 5 regression tests) ----
//
// gopls is not guaranteed to be on PATH (see TestManager_For_Match's
// t.Skip), but Manager.For's real behavior -- spawning a process, completing
// the initialize handshake over real stdio pipes, and detecting exit via
// cmd.Wait -- can only be exercised against an actual subprocess, not a
// mock. This uses the standard os/exec self-reexec trick (see TestHelperProcess
// in the Go standard library's os/exec tests): the compiled test binary
// re-execs itself with -test.run pinned to TestFakeLSPServerHelperProcess,
// which recognizes it is the re-exec'd child (via the "--" marker in
// os.Args, never present in a normal `go test` invocation) and becomes an
// LSP server speaking real Content-Length-framed JSON-RPC over its own
// stdin/stdout, using the same FakeServer already used for in-process tests.

// TestFakeLSPServerHelperProcess is not a real test. go test runs every
// TestXxx function it finds, so this must exist under that name, but it is a
// no-op unless fakeLSPServerMode reports this process was re-exec'd — see
// fakeLSPServerCmd.
func TestFakeLSPServerHelperProcess(t *testing.T) {
	mode := fakeLSPServerMode()
	if mode == "" {
		return
	}
	runFakeLSPServerHelperProcess(mode)
}

// fakeLSPServerMode extracts the mode passed after "--" in os.Args, or ""
// if this process is a normal `go test` run rather than the re-exec'd fake
// server child.
func fakeLSPServerMode() string {
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// runFakeLSPServerHelperProcess turns the current process into a minimal
// LSP server for exactly one of three modes:
//
//   - "crash": exits before completing the handshake at all -- the
//     scenario maxServerStarts exists to bound.
//   - "healthy": completes the handshake and stays up until the client
//     asks it to exit (LSP shutdown+exit, or its stdin closing), like a
//     deliberate lsp_restart shutting it down.
//   - "delayed-crash:<duration>": completes the handshake, stays up for
//     <duration>, then exits on its own, uninitiated by the client --
//     simulating a server that ran healthily for a while and then crashed.
func runFakeLSPServerHelperProcess(mode string) {
	if mode == "crash" {
		os.Exit(1)
	}

	fs := NewFakeServer()
	rw := &readWriteCloser{ReadCloser: os.Stdin, WriteCloser: os.Stdout}
	stream := jsonrpc2.NewBufferedStream(rw, jsonrpc2.VSCodeObjectCodec{})
	conn := jsonrpc2.NewConn(context.Background(), stream, fs)

	if d, ok := strings.CutPrefix(mode, "delayed-crash:"); ok {
		dur, err := time.ParseDuration(d)
		if err != nil {
			os.Exit(2)
		}
		select {
		case <-time.After(dur):
			os.Exit(1) // crash on our own, not because the client asked
		case <-fs.ShutdownCh:
		case <-conn.DisconnectNotify():
		}
		os.Exit(0)
	}

	// "healthy": run until told to stop.
	select {
	case <-fs.ShutdownCh:
	case <-conn.DisconnectNotify():
	}
	os.Exit(0)
}

// fakeLSPServerCmd returns a ServerConfig that re-execs this test binary as
// a fake LSP server in the given mode. See the block comment above.
func fakeLSPServerCmd(t *testing.T, mode string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return ServerConfig{
		Command:    exe,
		Args:       []string{"-test.run=^TestFakeLSPServerHelperProcess$", "--", mode},
		Extensions: []string{".fake"},
	}
}

// waitDead blocks until client reports dead, or fails the test after d.
func waitDead(t *testing.T, client *Client, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for !client.Dead() {
		select {
		case <-deadline:
			t.Fatal("server never exited")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestManager_RestartDoesNotConsumeBudget is the required regression test
// for Finding 5: lsp_restart deliberately restarting a healthy server more
// than maxServerStarts times must still hand back a working client. Before
// the fix, every spawn -- deliberate or not -- incremented restarts, so the
// initial start plus four lsp_restart calls exhausted the budget and every
// LSP tool failed for the rest of the process with a message ("failed to
// stay running") that was untrue: the server never failed on its own.
func TestManager_RestartDoesNotConsumeBudget(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"fake": fakeLSPServerCmd(t, "healthy"),
		},
	}, dir)
	defer mgr.Shutdown(context.Background())

	path := filepath.Join(dir, "main.fake")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Establish the initial server.
	if _, err := mgr.For(ctx, path); err != nil {
		t.Fatalf("initial For: %v", err)
	}

	// Restart it deliberately more times than maxServerStarts. Each
	// Restart shuts the current client down; For lazily respawns a fresh
	// "healthy" instance on the next call.
	for i := 0; i < maxServerStarts+4; i++ {
		if err := mgr.Restart(ctx, path); err != nil {
			t.Fatalf("Restart (call %d): %v", i, err)
		}
		client, err := mgr.For(ctx, path)
		if err != nil {
			t.Fatalf("For after Restart (call %d): %v -- a deliberate restart must not consume the crash budget", i, err)
		}
		if client == nil || client.Dead() {
			t.Fatalf("For after Restart (call %d): expected a live client", i)
		}
	}
}

// TestManager_CrashLoopStillTrips is the required companion to
// TestManager_RestartDoesNotConsumeBudget: a server that genuinely keeps
// failing to start on its own -- not via lsp_restart -- must still trip
// maxServerStarts. The fix for Finding 5 must not turn the crash budget
// into a no-op.
func TestManager_CrashLoopStillTrips(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"fake": fakeLSPServerCmd(t, "crash"),
		},
	}, dir)
	defer mgr.Shutdown(context.Background())

	path := filepath.Join(dir, "main.fake")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for i := 0; i < maxServerStarts; i++ {
		if _, err := mgr.For(ctx, path); err == nil {
			t.Fatalf("For (attempt %d): expected an error from a server that exits before handshake", i)
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Fatal("expected the crash loop to have produced errors")
	}

	// The budget is now exhausted: the next call must fail with the
	// "failed to stay running" message and must NOT attempt another spawn.
	_, err := mgr.For(ctx, path)
	if err == nil {
		t.Fatal("expected maxServerStarts to trip after a genuine crash loop")
	}
	if !strings.Contains(err.Error(), "failed to stay running") {
		t.Fatalf("got %q, want the budget-exhausted message", err)
	}
}

// TestManager_HealthyUptimeForgivesCrashBudget is the regression test for
// the other half of Finding 5's fix: a server that ran long enough to prove
// itself healthy and THEN crashed on its own must not have that crash
// permanently count toward the budget, or a long-lived process crashing
// occasionally would eventually lock LSP out for the rest of the session.
// Real healthyUptime is 2 minutes -- too slow for a unit test -- so this
// shrinks it and drives more than maxServerStarts self-inflicted
// "healthy for a while, then crash on its own" cycles through one manager.
// If forgiveness were missing, a cycle past maxServerStarts would return
// "has failed to stay running" even though every exit was preceded by a
// healthy run.
func TestManager_HealthyUptimeForgivesCrashBudget(t *testing.T) {
	origUptime := healthyUptime
	healthyUptime = 20 * time.Millisecond
	t.Cleanup(func() { healthyUptime = origUptime })

	dir := t.TempDir()
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"fake": fakeLSPServerCmd(t, "delayed-crash:80ms"),
		},
	}, dir)
	defer mgr.Shutdown(context.Background())

	path := filepath.Join(dir, "main.fake")

	for i := 0; i < maxServerStarts+3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		client, err := mgr.For(ctx, path)
		cancel()
		if err != nil {
			t.Fatalf("For (cycle %d): %v -- a self-crash after healthyUptime should have been forgiven", i, err)
		}
		if client == nil {
			t.Fatalf("For (cycle %d): nil client", i)
		}
		// Wait for this instance to exit on its own (past healthyUptime)
		// before asking again, so the next For call actually evicts and
		// respawns rather than handing back the still-live client.
		waitDead(t, client, 5*time.Second)
	}
}
