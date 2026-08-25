// Package integration_test contains end-to-end tests that build and run the
// rafikid daemon binary as a subprocess, communicate with it via UDS, and
// exercise the major control-protocol flows (spawn, list, get, kill, resume,
// subscribe, forget).
//
// Profile filtering (ctrl_subscribe with profile="coarse") is not tested here
// because profile→event-set expansion is not yet implemented (it is currently
// a no-op); add a test when that feature ships.
package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── TestMain: build binary once for all tests ────────────────────────────────

var (
	binaryPath string
	cliPath    string
	repoRoot   string
)

func TestMain(m *testing.M) {
	root, err := findModuleRoot()
	if err != nil {
		log.Fatalf("find module root: %v", err)
	}
	repoRoot = root

	binDir, err := os.MkdirTemp("", "rafiki-build")
	if err != nil {
		log.Fatalf("mkdirtemp for build: %v", err)
	}
	defer os.RemoveAll(binDir)

	// Both binaries land in the same directory, so their names must differ:
	// rafikid is the daemon, rafiki the client — and, since the executor folded
	// into it, also the executor via `rafiki executor serve`.
	for _, cmd := range []struct{ bin, pkg string }{
		{"rafikid", "./cmd/rafikid"},
		{"rafiki", "./cmd/rafiki"},
	} {
		out := filepath.Join(binDir, cmd.bin)
		build := exec.Command("go", "build", "-o", out, cmd.pkg)
		build.Dir = root
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			log.Fatalf("build %s: %v", cmd.bin, err)
		}
		switch cmd.bin {
		case "rafikid":
			binaryPath = out
		case "rafiki":
			cliPath = out
		}
	}

	os.Exit(m.Run())
}

// findModuleRoot walks up from the working directory until it finds go.mod.
func findModuleRoot() (string, error) {
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
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

// ─── daemon harness ───────────────────────────────────────────────────────────

// daemon wraps a running rafiki daemon subprocess with a temp HOME directory.
type daemon struct {
	socketPath string
	proc       *exec.Cmd
	homeDir    string
	// logsDir is the daemon's per-child log tree. Derived once here rather than
	// rebuilt at each use site, so a future layout move has one place to change.
	logsDir string
}

// bootDaemon starts the binary with a fresh temp HOME and a test DSN. It waits
// for the socket to appear before returning.
func bootDaemon(t *testing.T) *daemon {
	t.Helper()

	// On macOS the default temp dir resolves through /private/var/folders/…
	// making UDS paths exceed the 104-byte kernel limit. Use /tmp explicitly.
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	homeDir, err := os.MkdirTemp(base, "rafiki-it")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}

	// The daemon resolves every location through internal/paths, which is XDG —
	// deliberately NOT ~/.pi, which belongs to pi itself. Pin all three XDG bases
	// at the temp HOME so everything it writes lands inside the tree we clean up.
	// Setting HOME alone would also work (paths falls back to ~/.local/…) but
	// buries the socket three directories deeper, and sun_path has ~104 bytes to
	// spend. These must stay in step with internal/paths.
	appDir := filepath.Join(homeDir, "rafiki") // paths.base() appends the app leaf
	socketPath := filepath.Join(appDir, "controller.sock")
	if len(socketPath) > 100 {
		os.RemoveAll(homeDir)
		t.Fatalf("socket path too long (%d bytes) for UDS: %s", len(socketPath), socketPath)
	}

	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"XDG_RUNTIME_DIR="+homeDir,
		"XDG_STATE_HOME="+homeDir,
		"XDG_DATA_HOME="+homeDir,
		// The daemon requires a database (Phase C0). The suite is designed
		// around a disposable database, so use the RAFIKI_TEST_DSN the developer
		// supplies — it must be present, or the daemon cannot start at all.
		// A developer's ambient RAFIKI_DB would otherwise point this throwaway
		// daemon at their real conversations database.
		"RAFIKI_DB="+os.Getenv("RAFIKI_TEST_DSN"),
	)
	// Uncomment to stream daemon logs during debugging:
	// cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		os.RemoveAll(homeDir)
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemon{
		socketPath: socketPath,
		proc:       cmd,
		homeDir:    homeDir,
		logsDir:    filepath.Join(appDir, "logs"), // paths.LogsDir() == StateDir/logs
	}

	// Poll until the daemon is actually accepting. Stat-ing the socket path is
	// not enough: the file exists from the moment the listener is created, so a
	// stat-based wait races the daemon's own startup and the first dial gets
	// ECONNREFUSED. Connecting is the only proof there is something behind it.
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_ = conn.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		d.stopDaemon()
		t.Fatalf("daemon never accepted on %s: %v", socketPath, lastErr)
	}

	t.Cleanup(d.stopDaemon)
	return d
}

func (d *daemon) stopDaemon() {
	if d.proc != nil && d.proc.Process != nil {
		_ = d.proc.Process.Signal(syscall.SIGTERM)
		_ = d.proc.Wait()
	}
	os.RemoveAll(d.homeDir)
}

// request sends one JSONL frame on a fresh connection and returns the response.
func (d *daemon) request(t *testing.T, frame string) []byte {
	t.Helper()
	conn, err := net.Dial("unix", d.socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if _, err := fmt.Fprintln(conn, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return []byte(strings.TrimRight(line, "\n"))
}

// spawnChild sends ctrl_spawn with noSession:true (so resume works without a
// real session file) and returns the assigned childId.
// spawnChild sends ctrl_spawn with noSession:true (so resume works without a
// real session file) and returns the assigned childId. The child is an
// in-process fundi child, which requires a model; the throwaway model string
// is never actually sent to a provider — these tests only exercise the
// daemon's spawn/kill/subscribe lifecycle.
func (d *daemon) spawnChild(t *testing.T) string {
	t.Helper()
	raw := d.request(t, `{"type":"ctrl_spawn","id":"spawn","cwd":"/tmp","noSession":true,"kind":"fundi","model":"anthropic/sonnet-latest"}`)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_spawn failed: %+v", r.Error)
	}
	var data protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &data)
	if data.ChildID == "" {
		t.Fatal("spawn returned empty childId")
	}
	return data.ChildID
}

// ─── subscriber connection ────────────────────────────────────────────────────

// subConn is a persistent UDS connection that reads frames asynchronously.
// ctrl_response frames are kept in a separate FIFO so that nextResponse()
// can drain them in order without accidentally consuming event frames.
type subConn struct {
	t         *testing.T
	conn      net.Conn
	br        *bufio.Reader
	mu        sync.Mutex
	responses []json.RawMessage // ctrl_response frames
	events    []json.RawMessage // all other frames (child events, lifecycle events)
}

// dial opens a persistent connection to the daemon for subscription use.
func (d *daemon) dial(t *testing.T) *subConn {
	t.Helper()
	conn, err := net.Dial("unix", d.socketPath)
	if err != nil {
		t.Fatalf("dial sub: %v", err)
	}
	sc := &subConn{
		t:    t,
		conn: conn,
		br:   bufio.NewReader(conn),
	}
	go sc.readLoop()
	t.Cleanup(func() { conn.Close() })
	return sc
}

func (sc *subConn) readLoop() {
	for {
		line, err := sc.br.ReadString('\n')
		if err != nil {
			return // connection closed
		}
		frame := json.RawMessage(strings.TrimRight(line, "\n"))
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(frame, &hdr); err != nil {
			// Logf, not Fatal: this runs in a background goroutine, and
			// FailNow-family calls are only safe from the test's own
			// goroutine.
			sc.t.Logf("subConn readLoop: malformed frame %q: %v", frame, err)
		}

		sc.mu.Lock()
		if hdr.Type == "ctrl_response" {
			sc.responses = append(sc.responses, frame)
		} else {
			sc.events = append(sc.events, frame)
		}
		sc.mu.Unlock()
	}
}

// send writes a JSONL frame to the connection.
func (sc *subConn) send(frame string) {
	sc.t.Helper()
	if err := sc.conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		sc.t.Fatalf("subConn set deadline: %v", err)
	}
	if _, err := fmt.Fprintln(sc.conn, frame); err != nil {
		sc.t.Fatalf("subConn write: %v", err)
	}
}

// nextResponse waits for (and removes) the next ctrl_response from the queue.
func (sc *subConn) nextResponse(t *testing.T, timeout time.Duration) protocol.Response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sc.mu.Lock()
		if len(sc.responses) > 0 {
			raw := sc.responses[0]
			sc.responses = sc.responses[1:]
			sc.mu.Unlock()
			var resp protocol.Response
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal ctrl_response: %v\ndata: %s", err, raw)
			}
			return resp
		}
		sc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout (%v) waiting for ctrl_response", timeout)
	return protocol.Response{}
}

// waitForEvent polls the event buffer until predicate returns true, then
// returns the matching frame. Fails the test on timeout.
func (sc *subConn) waitForEvent(t *testing.T, predicate func(json.RawMessage) bool, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sc.mu.Lock()
		for _, f := range sc.events {
			if predicate(f) {
				sc.mu.Unlock()
				return f
			}
		}
		sc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	sc.mu.Lock()
	n := len(sc.events)
	sc.mu.Unlock()
	t.Fatalf("timeout (%v) waiting for matching event; received %d event(s)", timeout, n)
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v\ndata: %s", err, data)
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestIntegration_FullLifecycle exercises the canonical flow:
// spawn → send prompt → kill → confirm exited → forget.
func TestIntegration_FullLifecycle(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Send a frame to the child; the daemon's ctrl_send ack is what this asserts.
	sendJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"s1","childId":%q,"frame":{"type":"get_state","id":"u1"}}`,
		childID,
	)
	raw := d.request(t, sendJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Errorf("ctrl_send failed: %+v", r.Error)
	}

	// Kill.
	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	raw = d.request(t, killJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_kill failed: %+v", r.Error)
	}

	// Confirm exited via ctrl_list.
	raw = d.request(t, `{"type":"ctrl_list","id":"l1"}`)
	mustUnmarshal(t, raw, &r)
	var listData protocol.ListResponseData
	mustUnmarshal(t, r.Data, &listData)

	var found *protocol.ChildSummary
	for i, c := range listData.Children {
		if c.ChildID == childID {
			found = &listData.Children[i]
		}
	}
	if found == nil {
		t.Fatal("child not found in ctrl_list after kill")
	}
	if found.Status != string(protocol.StatusExited) {
		t.Errorf("want status=%s, got %s", protocol.StatusExited, found.Status)
	}

	// Forget.
	forgetJSON := fmt.Sprintf(`{"type":"ctrl_forget","id":"f1","childId":%q}`, childID)
	raw = d.request(t, forgetJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_forget failed: %+v", r.Error)
	}

	// Verify the child is gone from ctrl_list.
	raw = d.request(t, `{"type":"ctrl_list","id":"l2"}`)
	mustUnmarshal(t, raw, &r)
	mustUnmarshal(t, r.Data, &listData)
	for _, c := range listData.Children {
		if c.ChildID == childID {
			t.Error("child still present in ctrl_list after ctrl_forget")
		}
	}
}

// TestIntegration_KillResume exercises kill+resume:
// spawn → kill → confirm exited → resume → same childId.
func TestIntegration_KillResume(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Capture the initial PID via ctrl_get.
	getJSON := fmt.Sprintf(`{"type":"ctrl_get","id":"g1","childId":%q}`, childID)
	raw := d.request(t, getJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_get failed: %+v", r.Error)
	}
	var child1 protocol.ChildSummary
	mustUnmarshal(t, r.Data, &child1)
	if child1.Status == string(protocol.StatusExited) {
		t.Fatal("child should be alive after spawn")
	}

	// Kill.
	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	raw = d.request(t, killJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_kill failed: %+v", r.Error)
	}

	// Confirm exited.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var exitedChild protocol.ChildSummary
	mustUnmarshal(t, r.Data, &exitedChild)
	if exitedChild.Status != string(protocol.StatusExited) {
		t.Fatalf("want status=exited after kill, got %s", exitedChild.Status)
	}

	// Resume — should re-spawn with the same childId.
	resumeJSON := fmt.Sprintf(`{"type":"ctrl_resume","id":"re1","childId":%q}`, childID)
	raw = d.request(t, resumeJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_resume failed: %+v", r.Error)
	}
	var resumeData protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &resumeData)
	if resumeData.ChildID != childID {
		t.Errorf("resume: want childId=%s, got %s", childID, resumeData.ChildID)
	}

	// The resumed child must be alive and, for a kind with a real OS process
	// (claude), have a different PID. An in-process fundi child has PID 0 and
	// never forks, so the "different PID" assertion is only meaningful when the
	// original child had a real PID.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var child2 protocol.ChildSummary
	mustUnmarshal(t, r.Data, &child2)
	if child2.Status == string(protocol.StatusExited) {
		t.Fatal("resumed child should be alive, not exited")
	}
	if child1.PID != nil && child2.PID != nil && *child1.PID != 0 && *child1.PID == *child2.PID {
		t.Error("resumed child should have a different PID from the original")
	}
}

// TestIntegration_LogDumpOnExit verifies that all four log files are written
// under the daemon's logs dir (paths.LogsDir()/<childId>/) when a child exits
// (Fix 3 / spec §11.3).
func TestIntegration_LogDumpOnExit(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Kill the child to trigger handleChildExit → LogDumper.Dump.
	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	raw := d.request(t, killJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_kill failed: %+v", r.Error)
	}

	// Give the daemon a moment to finish the dump (kill returns after process
	// exit, but handleChildExit runs in monitorChild which is concurrent).
	// Poll for err.log.gz — the last file Dump writes — to avoid a race where
	// meta.json appears before the gz files are flushed.
	childLogDir := filepath.Join(d.logsDir, childID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(childLogDir, "err.log.gz")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, name := range []string{"meta.json", "in.jsonl.gz", "out.jsonl.gz", "err.log.gz"} {
		path := filepath.Join(childLogDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("log file missing: %s (%v)", name, err)
		}
	}
}

// TestIntegration_ResumeEmitsSpawned verifies that ctrl_resume emits
// ctrl_child_spawned (Fix 2 / spec §7.2).
func TestIntegration_ResumeEmitsSpawned(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Kill the child.
	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	raw := d.request(t, killJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_kill failed: %+v", r.Error)
	}

	// Set up a global subscriber to catch the ctrl_child_spawned event.
	sc := d.dial(t)
	sc.send(`{"type":"ctrl_global_subscribe","id":"gsub1"}`)
	gsubResp := sc.nextResponse(t, 5*time.Second)
	if !gsubResp.Success {
		t.Fatalf("ctrl_global_subscribe failed: %+v", gsubResp.Error)
	}

	// Resume.
	resumeJSON := fmt.Sprintf(`{"type":"ctrl_resume","id":"re1","childId":%q}`, childID)
	raw = d.request(t, resumeJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_resume failed: %+v", r.Error)
	}

	// Global subscriber must receive ctrl_child_spawned for the resumed child.
	sc.waitForEvent(t, func(f json.RawMessage) bool {
		var ev struct {
			Type    string `json:"type"`
			ChildID string `json:"childId"`
		}
		if json.Unmarshal(f, &ev) != nil {
			return false
		}
		return ev.Type == protocol.TypeCtrlChildSpawned && ev.ChildID == childID
	}, 5*time.Second)
}
