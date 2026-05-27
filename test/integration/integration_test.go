// Package integration_test contains end-to-end tests that build and run the
// pi-controller binary as a subprocess, communicate with it via UDS using
// fake-pi.sh as the pi binary, and exercise the major protocol flows.
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

	"graveland.dev/pi-controller/protocol"
)

// ─── TestMain: build binary once for all tests ────────────────────────────────

var (
	binaryPath  string
	piCtlPath   string
	fakePiPath  string
	repoRoot    string
)

func TestMain(m *testing.M) {
	root, err := findModuleRoot()
	if err != nil {
		log.Fatalf("find module root: %v", err)
	}
	repoRoot = root

	fakePiPath = filepath.Join(root, "test", "integration", "fake-pi.sh")

	binDir, err := os.MkdirTemp("", "pic-build")
	if err != nil {
		log.Fatalf("mkdirtemp for build: %v", err)
	}
	defer os.RemoveAll(binDir)

	for _, cmd := range []struct{ bin, pkg string }{
		{"pi-controller", "./cmd/pi-controller"},
		{"pic", "./cmd/pic"},
	} {
		out := filepath.Join(binDir, cmd.bin)
		build := exec.Command("go", "build", "-o", out, cmd.pkg)
		build.Dir = root
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			log.Fatalf("build %s: %v", cmd.bin, err)
		}
		switch cmd.bin {
		case "pi-controller":
			binaryPath = out
		case "pic":
			piCtlPath = out
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

// daemon wraps a running pi-controller subprocess with a temp HOME directory.
type daemon struct {
	socketPath string
	proc       *exec.Cmd
	homeDir    string
}

// bootDaemon starts the binary with a fresh temp HOME and fake-pi.sh as the
// pi binary. It waits for the socket to appear before returning.
func bootDaemon(t *testing.T) *daemon {
	t.Helper()

	// On macOS the default temp dir resolves through /private/var/folders/…
	// making UDS paths exceed the 104-byte kernel limit. Use /tmp explicitly.
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	homeDir, err := os.MkdirTemp(base, "pic")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}

	socketPath := filepath.Join(homeDir, ".pi", "run", "controller.sock")
	if len(socketPath) > 100 {
		os.RemoveAll(homeDir)
		t.Fatalf("socket path too long (%d bytes) for UDS: %s", len(socketPath), socketPath)
	}

	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"PI_BINARY="+fakePiPath,
	)
	// Uncomment to stream daemon logs during debugging:
	// cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		os.RemoveAll(homeDir)
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemon{socketPath: socketPath, proc: cmd, homeDir: homeDir}

	// Poll until the socket file appears (daemon creates it when ready).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(socketPath); err != nil {
		d.stopDaemon()
		t.Fatalf("daemon socket never appeared: %v", err)
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
	conn.SetDeadline(time.Now().Add(15 * time.Second))

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
func (d *daemon) spawnChild(t *testing.T) string {
	t.Helper()
	raw := d.request(t, `{"type":"ctrl_spawn","id":"spawn","cwd":"/tmp","noSession":true}`)
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
	events    []json.RawMessage // all other frames (pi events, lifecycle events)
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
		json.Unmarshal(frame, &hdr)

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
	sc.conn.SetDeadline(time.Now().Add(15 * time.Second))
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
			json.Unmarshal(raw, &resp)
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

func frameType(f json.RawMessage) string {
	var hdr struct{ Type string `json:"type"` }
	json.Unmarshal(f, &hdr)
	return hdr.Type
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestIntegration_FullLifecycle exercises the canonical flow:
// spawn → send prompt → kill → confirm exited → forget.
func TestIntegration_FullLifecycle(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Send a frame to the child. fake-pi.sh returns a generic success response.
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

// TestIntegration_SubscribeEvents exercises per-child subscriptions:
// spawn → subscribe → cause pi events → verify subscriber receives them.
func TestIntegration_SubscribeEvents(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Persistent connection for subscription.
	sc := d.dial(t)
	sc.send(fmt.Sprintf(`{"type":"ctrl_subscribe","id":"sub1","childId":%q}`, childID))
	subResp := sc.nextResponse(t, 5*time.Second)
	if !subResp.Success {
		t.Fatalf("ctrl_subscribe failed: %+v", subResp.Error)
	}

	// Trigger an agent_start event via the fake-pi test helper.
	emitJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"ev1","childId":%q,"frame":{"type":"__ctrl_test_emit","eventType":"agent_start"}}`,
		childID,
	)
	// Use a fresh connection so its ctrl_response does not land in sc.
	raw := d.request(t, emitJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (__ctrl_test_emit) failed: %+v", r.Error)
	}

	// Subscriber must receive the agent_start event wrapped in a ctrl_event
	// envelope (spec §7.1). The outer type is "ctrl_event" and the inner
	// event.type is "agent_start".
	sc.waitForEvent(t, func(f json.RawMessage) bool {
		var env struct {
			Type    string          `json:"type"`
			ChildID string          `json:"childId"`
			Event   json.RawMessage `json:"event"`
		}
		if json.Unmarshal(f, &env) != nil || env.Type != "ctrl_event" || env.ChildID != childID {
			return false
		}
		var inner struct{ Type string `json:"type"` }
		json.Unmarshal(env.Event, &inner)
		return inner.Type == "agent_start"
	}, 5*time.Second)
}

// TestIntegration_KillResume exercises kill+resume:
// spawn → kill → confirm exited → resume → same childId, new process.
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

	// The resumed child must be alive and have a different PID.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var child2 protocol.ChildSummary
	mustUnmarshal(t, r.Data, &child2)
	if child2.Status == string(protocol.StatusExited) {
		t.Fatal("resumed child should be alive, not exited")
	}
	if child1.PID != nil && child2.PID != nil && *child1.PID == *child2.PID {
		t.Error("resumed child should have a different PID from the original")
	}
}

// TestIntegration_InterceptionNewSession exercises the new_session interception
// path: spawn → subscribe → ctrl_send {type:new_session} → verify the child
// was transparently killed and re-spawned (same childId, new PID, fresh
// sessionId+sessionFile), and that the subscriber receives the synthesized pi
// response wrapped in a ctrl_event envelope.
func TestIntegration_InterceptionNewSession(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// Spawn WITHOUT noSession:true so we get a real session file. This lets
	// the test verify that new_session creates a fresh session (not the old one).
	// fake-pi.sh returns a unique sessionId/sessionFile per invocation unless
	// --session is passed, which is exactly what the buggy Resume path does.
	raw := d.request(t, `{"type":"ctrl_spawn","id":"spawn1","cwd":"/tmp"}`)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_spawn failed: %+v", r.Error)
	}
	var spawnData protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &spawnData)
	childID := spawnData.ChildID

	// Capture initial state including sessionId and sessionFile.
	getJSON := fmt.Sprintf(`{"type":"ctrl_get","id":"g1","childId":%q}`, childID)
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var child1 protocol.ChildSummary
	mustUnmarshal(t, r.Data, &child1)

	// Persistent subscriber — subscribes before the intercept fires so the
	// preserved subscription receives the synthesized pi response.
	sc := d.dial(t)
	sc.send(fmt.Sprintf(`{"type":"ctrl_subscribe","id":"sub1","childId":%q}`, childID))
	subResp := sc.nextResponse(t, 5*time.Second)
	if !subResp.Success {
		t.Fatalf("ctrl_subscribe failed: %+v", subResp.Error)
	}

	// Send new_session — the controller intercepts this and performs kill+respawn.
	// RespawnChild must NOT pass --session so pi creates a fresh session.
	interceptJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"int1","childId":%q,"frame":{"type":"new_session","id":"pi-req-1"}}`,
		childID,
	)
	raw = d.request(t, interceptJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (new_session) failed: %+v", r.Error)
	}

	// After ctrl_send returns the child must be alive again with the same childId.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var child2 protocol.ChildSummary
	mustUnmarshal(t, r.Data, &child2)

	if child2.Status == string(protocol.StatusExited) {
		t.Fatal("child should be alive after interception (re-spawned)")
	}
	if child2.ChildID != childID {
		t.Errorf("childId changed after interception: want %s, got %s", childID, child2.ChildID)
	}
	if child1.PID != nil && child2.PID != nil && *child1.PID == *child2.PID {
		t.Errorf("expected different PID after interception (new process): both are %d", *child1.PID)
	}
	// new_session must produce a fresh session (different file and id).
	// fake-pi.sh returns a --session-path-derived id when --session is passed
	// (old buggy Resume path), but a PID-based id when no --session is given
	// (correct RespawnChild path), so these assertions catch the regression.
	if child1.SessionFile != "" && child2.SessionFile == child1.SessionFile {
		t.Errorf("new_session should create a fresh session file; still using %s", child1.SessionFile)
	}
	if child1.SessionID != "" && child2.SessionID == child1.SessionID {
		t.Errorf("new_session should have a fresh sessionId; still using %s", child1.SessionID)
	}

	// The subscriber must receive the synthesized pi response for new_session,
	// wrapped in a ctrl_event envelope (spec §7.1).
	sc.waitForEvent(t, func(f json.RawMessage) bool {
		var env struct {
			Type    string          `json:"type"`
			ChildID string          `json:"childId"`
			Event   json.RawMessage `json:"event"`
		}
		if json.Unmarshal(f, &env) != nil || env.Type != "ctrl_event" || env.ChildID != childID {
			return false
		}
		var inner struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		json.Unmarshal(env.Event, &inner)
		return inner.Type == "response" && inner.Command == "new_session"
	}, 5*time.Second)
}

// TestIntegration_GetRecent verifies that ctrl_get_recent returns buffered
// events and that the include filter selects only matching types.
func TestIntegration_GetRecent(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// The bootstrap get_state response from fake-pi is already in the ring buffer.
	recentJSON := fmt.Sprintf(`{"type":"ctrl_get_recent","id":"gr1","childId":%q}`, childID)
	raw := d.request(t, recentJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_get_recent failed: %+v", r.Error)
	}
	var recentData protocol.GetRecentResponseData
	mustUnmarshal(t, r.Data, &recentData)
	if len(recentData.Events) == 0 {
		t.Fatal("expected events in ring buffer immediately after spawn")
	}

	// The bootstrap get_state response must be present.
	foundBootstrap := false
	for _, ev := range recentData.Events {
		var hdr struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		json.Unmarshal(ev, &hdr)
		if hdr.Type == "response" && hdr.Command == "get_state" {
			foundBootstrap = true
		}
	}
	if !foundBootstrap {
		t.Error("expected bootstrap get_state response in ring buffer")
	}

	// Push two typed events using the fake-pi test helper.
	for i, evt := range []string{"agent_start", "agent_end"} {
		emitJSON := fmt.Sprintf(
			`{"type":"ctrl_send","id":"ev%d","childId":%q,"frame":{"type":"__ctrl_test_emit","eventType":%q}}`,
			i, childID, evt,
		)
		d.request(t, emitJSON)
	}

	// Poll ctrl_get_recent with include:["agent_start"] until the event appears.
	// The emit is async: fake-pi stdout is read in a separate goroutine.
	filteredJSON := fmt.Sprintf(
		`{"type":"ctrl_get_recent","id":"gr2","childId":%q,"include":["agent_start"]}`,
		childID,
	)
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		raw = d.request(t, filteredJSON)
		mustUnmarshal(t, raw, &r)
		var fd protocol.GetRecentResponseData
		mustUnmarshal(t, r.Data, &fd)

		// Any returned events must match the include filter.
		for _, ev := range fd.Events {
			if tp := frameType(ev); tp != "agent_start" {
				t.Errorf("filtered get_recent returned unexpected type %q", tp)
			}
		}
		if len(fd.Events) > 0 {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Error("expected agent_start events in filtered ctrl_get_recent result")
	}
}

// TestIntegration_PerChildStatusEvents verifies that ctrl_child_status events
// are delivered to per-child subscribers (Fix 1 / spec §7.4).
func TestIntegration_PerChildStatusEvents(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Per-child subscriber.
	sc := d.dial(t)
	sc.send(fmt.Sprintf(`{"type":"ctrl_subscribe","id":"sub1","childId":%q}`, childID))
	subResp := sc.nextResponse(t, 5*time.Second)
	if !subResp.Success {
		t.Fatalf("ctrl_subscribe failed: %+v", subResp.Error)
	}

	// Trigger agent_start → the controller should emit ctrl_child_status
	// streaming→idle (via monitorChild) and deliver it to sc.
	emitJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"ev1","childId":%q,"frame":{"type":"__ctrl_test_emit","eventType":"agent_start"}}`,
		childID,
	)
	d.request(t, emitJSON)

	// The per-child subscriber must receive ctrl_child_status with
	// status="streaming" (delivered directly, not wrapped in ctrl_event).
	sc.waitForEvent(t, func(f json.RawMessage) bool {
		var ev struct {
			Type    string `json:"type"`
			ChildID string `json:"childId"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(f, &ev) != nil {
			return false
		}
		return ev.Type == protocol.TypeCtrlChildStatus &&
			ev.ChildID == childID &&
			ev.Status == string(protocol.StatusStreaming)
	}, 5*time.Second)
}

// TestIntegration_LogDumpOnExit verifies that all four log files are written
// under ~/.pi/run/logs/<childId>/ when a child exits (Fix 3 / spec §11.3).
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
	childLogDir := filepath.Join(d.homeDir, ".pi", "run", "logs", childID)
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

// TestIntegration_GetRecentExited verifies that ctrl_get_recent returns events
// for an exited child via the ring snapshot (Fix 4 / spec §11.4).
func TestIntegration_GetRecentExited(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	childID := d.spawnChild(t)

	// Populate the ring with a known event.
	emitJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"ev1","childId":%q,"frame":{"type":"__ctrl_test_emit","eventType":"agent_start"}}`,
		childID,
	)
	d.request(t, emitJSON)

	// Kill the child.
	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	raw := d.request(t, killJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_kill failed: %+v", r.Error)
	}

	// Wait for the child to be exited in the store.
	getJSON := fmt.Sprintf(`{"type":"ctrl_get","id":"g1","childId":%q}`, childID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw = d.request(t, getJSON)
		mustUnmarshal(t, raw, &r)
		var cs protocol.ChildSummary
		mustUnmarshal(t, r.Data, &cs)
		if cs.Status == string(protocol.StatusExited) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ctrl_get_recent on an exited child must return events from the snapshot.
	recentJSON := fmt.Sprintf(`{"type":"ctrl_get_recent","id":"gr1","childId":%q}`, childID)
	raw = d.request(t, recentJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_get_recent failed: %+v", r.Error)
	}
	var recentData protocol.GetRecentResponseData
	mustUnmarshal(t, r.Data, &recentData)
	if len(recentData.Events) == 0 {
		t.Error("expected events in ring snapshot for exited child; got none")
	}

	// Must contain the bootstrap get_state response at minimum.
	foundBootstrap := false
	for _, ev := range recentData.Events {
		var hdr struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		json.Unmarshal(ev, &hdr)
		if hdr.Type == "response" && hdr.Command == "get_state" {
			foundBootstrap = true
		}
	}
	if !foundBootstrap {
		t.Error("expected bootstrap get_state response in exited child ring snapshot")
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
		json.Unmarshal(f, &ev)
		return ev.Type == protocol.TypeCtrlChildSpawned && ev.ChildID == childID
	}, 5*time.Second)
}
