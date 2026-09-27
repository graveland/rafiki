// Package integration_test contains end-to-end tests that build and run the
// rafikid daemon binary as a subprocess, communicate with it over its
// the daemon's control socket through the generated Connect client
// (pkg/gen/rafiki/v1/rafikiv1connect), and exercise the major control flows
// (spawn, list, get, kill, resume, stream events, close).
//
// Event-tier filtering (StreamEvents with a profile-style type restriction)
// is not tested here because profile→event-set expansion is not yet
// implemented (it is currently a no-op); add a test when that feature ships.
package integration_test

import (
	"context"
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

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
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
	// stderr captures the daemon process's stderr. bootDaemonDB always sets it:
	// the proxy face announces its resolved port only on stderr, and every boot
	// failure carries the tail. bootDaemon leaves it nil and discards stderr.
	stderr *stderrBuf
	// proxyURL is the proxy face's resolved loopback URL. Set by bootDaemonDB,
	// which waits for the face's announce line before returning; zero for
	// bootDaemon, whose tests do not dial the face.
	proxyURL string
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

	// Name the daemon rather than letting it mint an id, so its child rows can
	// be swept on cleanup: they outlive homeDir, and every later daemon in the
	// suite would otherwise recover and auto-resume them. See dropDaemonRows.
	daemonID := nextDaemonID()
	dropDaemonRows(t, daemonID)

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
		// Executors must be off for the suite's plain spawns: a DB-backed daemon
		// builds an executor pool by default (no RAFIKI_CONTROL_LISTEN, so
		// executorsEnabled defaults on), and a top-level empty-selector spawn
		// against a pool with zero live executors is now REFUSED
		// ("no executor satisfies \"\"") instead of starting toolless. Tests
		// that want the executor plane boot their own daemon with
		// RAFIKI_EXECUTORS_ENABLED=1 (grant_test) or enroll an executor first.
		"RAFIKI_EXECUTORS_ENABLED=0",
		// Each daemon binds its OWN ephemeral proxy port. A fixed
		// RAFIKI_PROXY_LISTEN (or the :8035 default) makes the suite's parallel
		// daemons collide with each other — the first to bind wins, the rest
		// log "address already in use" and come up degraded, and the two
		// Connect-plane tests fail because the Connect UDS is served only when
		// the proxy face comes up. Port 0 lets the kernel pick, and the face
		// resolves the real port for its children's URLs.
		"RAFIKI_PROXY_LISTEN=127.0.0.1:0",
		"RAFIKI_DAEMON_ID="+daemonID,
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

// ─── Connect client ───────────────────────────────────────────────────────────

// control returns the generated Connect client for the daemon's control
// socket. The UDS is the credential: the listener admits anonymous local
// callers (optionalIdentityInterceptor), so no bearer token is needed here.
// The dial shape lives on connectClient (cockpit_subject_test.go) and must
// match cmd/rafiki/connectclient.go.
func (d *daemon) control(t *testing.T) rafikiv1connect.ControlClient {
	t.Helper()
	return d.connectClient()
}

// spawnChild spawns a fundi child with noSession:true (so resume works without
// a real session file) over the daemon's control socket and returns the assigned
// childId. The child is an in-process fundi child, which requires a model; the
// throwaway model string is never actually sent to a provider — these tests
// only exercise the daemon's spawn/kill/stream/close lifecycle.
func (d *daemon) spawnChild(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       "/tmp",
		NoSession: true,
		Kind:      protocol.KindFundi,
		Model:     "anthropic/sonnet-latest",
	}))
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if resp.Msg.GetChildId() == "" {
		t.Fatal("spawn returned empty childId")
	}
	return resp.Msg.GetChildId()
}

// ─── Connect call helpers ─────────────────────────────────────────────────────

// getChild returns one child's summary over Connect, failing the test when the
// child is unknown or the call errors.
func getChild(t *testing.T, client rafikiv1connect.ControlClient, childID string) *rafikiv1.ChildSummary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		t.Fatalf("GetChild(%s): %v", childID, err)
	}
	return resp.Msg.GetChild()
}

// listChildren returns every child over one Connect call.
func listChildren(t *testing.T, client rafikiv1connect.ControlClient) []*rafikiv1.ChildSummary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	return resp.Msg.GetChildren()
}

// ─── event subscription ───────────────────────────────────────────────────────

// childEventsSubject names one child — the native per-child subscription.
func childEventsSubject(childID string) *rafikiv1.EventSubject {
	return &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}}
}

// allEventsSubject names everything the caller is entitled to — the native
// global subscription.
func allEventsSubject() *rafikiv1.EventSubject {
	return &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}}
}

// eventWatermark returns the child's latest durable event-log ordinal. A
// stream opened with a replay cursor delivers every stored event with an
// ordinal above the cursor value, so a test that must see only post-open
// events takes the watermark first and excludes ordinals at or below it —
// every event appended after the open has a strictly greater ordinal.
func eventWatermark(t *testing.T, client rafikiv1connect.ControlClient, childID string) int32 {
	t.Helper()
	latest := getChild(t, client, childID).LatestOrdinal
	if latest == nil {
		t.Fatalf("child %s carries no event-log ordinal; nothing to replay from", childID)
	}
	return *latest
}

// eventStream is an open StreamEvents server stream whose messages a
// background goroutine buffers as they arrive, so a test can wait for a
// matching event without consuming the ones before it.
type eventStream struct {
	stream *connect.ServerStreamForClient[rafikiv1.Event]

	// watermark is the subject child's latest durable ordinal at open time.
	// Every replayed event carries an ordinal at or below it (the replay
	// cursor covers the child's already-stored tail); every event appended
	// after the open carries a strictly greater one — or none, when its
	// best-effort log append failed. waits() admits only the latter two.
	watermark int32

	mu     sync.Mutex
	events []*rafikiv1.Event
}

// openEvents opens a durable-tier event stream over the control socket and starts
// buffering it. replay maps child id → last ordinal SEEN (-1 replays the
// child's whole log); pass the watermark for a minimal one-event replay.
//
// The cursor is load-bearing, and not because the tests want replay: a
// from-now stream sends no response headers until its first live message,
// so a test that opened one synchronously before the action it watches
// would deadlock on the open itself (the server-side subscription only
// attaches after the replay read). The replays also make the subscription
// itself durable: an event published while the open was in flight is
// appended to the log before it is published anywhere, so a replay that
// starts late still covers it — the same property the framed plane's
// subscribe-ack gave, minus the microseconds between the replay read and
// the live subscription attaching.
func (d *daemon) openEvents(t *testing.T, subject *rafikiv1.EventSubject, replay map[string]int32, watermark int32) *eventStream {
	t.Helper()
	// Long-lived on purpose: the stream is open for the whole test and every
	// individual wait carries its own timeout. Cancelled in cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := d.control(t).StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: subject,
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
		Cursor:  &rafikiv1.EventCursor{Ordinals: replay},
	}))
	if err != nil {
		cancel()
		t.Fatalf("StreamEvents: %v", err)
	}
	es := &eventStream{stream: stream, watermark: watermark}
	go func() {
		for stream.Receive() {
			es.mu.Lock()
			es.events = append(es.events, stream.Msg())
			es.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = stream.Close()
	})
	return es
}

// isLive reports whether ev was appended after the stream opened: its
// ordinal is strictly above the watermark, or it carries none (a failed
// best-effort log append — still a real live event, just an unresumable
// one). Replayed events always carry their stored ordinal.
func (es *eventStream) isLive(ev *rafikiv1.Event) bool {
	if ord := ev.Ordinal; ord != nil && *ord <= es.watermark {
		return false
	}
	return true
}

// dumpLocked renders every buffered event, newest index last; caller holds
// es.mu.
func (es *eventStream) dumpLocked() string {
	dump := make([]string, 0, len(es.events))
	for i, e := range es.events {
		b, err := protojson.Marshal(e)
		if err != nil {
			b = []byte(fmt.Sprintf("%+v", e))
		}
		dump = append(dump, fmt.Sprintf("  [%d] %s", i, b))
	}
	return strings.Join(dump, "\n")
}

// waitEvent blocks until es's buffer contains a LIVE event matching predicate
// and returns it, failing the test on timeout. It never consumes a
// non-matching event, so several waits can share one stream.
func (es *eventStream) waitEvent(t *testing.T, predicate func(*rafikiv1.Event) bool, timeout time.Duration) *rafikiv1.Event {
	t.Helper()
	ev, _ := es.waitEventAfter(t, 0, predicate, timeout)
	return ev
}

// waitEventAfter scans es's buffer from index `from` until it finds a live
// event matching predicate, returning the event and the index just past it.
// The cursor form exists because the same event type recurs across turns on
// one child — a wait for the SECOND occurrence must not succeed instantly on
// the first. On timeout it dumps every buffered event: a wait that says only
// "I didn't find it" leaves the reader guessing about which of "never
// emitted", "emitted before `from`", or "emitted in a different shape"
// happened.
func (es *eventStream) waitEventAfter(t *testing.T, from int, predicate func(*rafikiv1.Event) bool, timeout time.Duration) (*rafikiv1.Event, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		es.mu.Lock()
		for i := from; i < len(es.events); i++ {
			if es.isLive(es.events[i]) && predicate(es.events[i]) {
				ev := es.events[i]
				es.mu.Unlock()
				return ev, i + 1
			}
		}
		es.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	t.Fatalf("timeout (%v) waiting for matching event after index %d; buffered events:\n%s",
		timeout, from, es.dumpLocked())
	return nil, from
}

// agentStatusEvent matches a durable agent_status event for childID carrying
// the wanted state. It is the native witness of a turn boundary: the framed
// plane's inner agent_start/agent_settled events are one state machine here
// (pkg/child/state.go — agent_start drives streaming, agent_settled drives
// idle), and handleStatusChange (cmd/rafikid/controller.go) is the daemon's
// only agent_status producer, fed by a DRAINED transition queue, so neither
// end of a fast turn can be lost the way the old sampled status frames could.
func agentStatusEvent(childID, state string) func(*rafikiv1.Event) bool {
	return func(ev *rafikiv1.Event) bool {
		st := ev.GetAgentStatus()
		return ev.GetChildId() == childID && st != nil && st.GetState() == state
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestIntegration_FullLifecycle exercises the canonical flow:
// spawn → send frame → kill → confirm exited → close (forget).
func TestIntegration_FullLifecycle(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	client := d.control(t)

	childID := d.spawnChild(t)

	// Send a raw child-protocol frame; the SendFrame ack is what this asserts.
	sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
	if _, err := client.SendFrame(sctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"get_state","id":"u1"}`,
	})); err != nil {
		t.Errorf("SendFrame failed: %v", err)
	}
	scancel()

	// Kill.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID})); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	kcancel()

	// Confirm exited via ListChildren.
	var found *rafikiv1.ChildSummary
	for _, c := range listChildren(t, client) {
		if c.GetChildId() == childID {
			found = c
		}
	}
	if found == nil {
		t.Fatal("child not found in ListChildren after kill")
	}
	if found.GetStatus() != string(protocol.StatusExited) {
		t.Errorf("want status=%s, got %s", protocol.StatusExited, found.GetStatus())
	}

	// Close (forget).
	cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
	if _, err := client.Close(cctx, connect.NewRequest(&rafikiv1.CloseRequest{ChildId: childID})); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	ccancel()

	// Verify the child is gone from ListChildren.
	for _, c := range listChildren(t, client) {
		if c.GetChildId() == childID {
			t.Error("child still present in ListChildren after Close")
		}
	}
}

// TestIntegration_KillResume exercises kill+resume:
// spawn → kill → confirm exited → resume → same childId.
func TestIntegration_KillResume(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	client := d.control(t)

	childID := d.spawnChild(t)

	// Capture the initial state via GetChild.
	child1 := getChild(t, client, childID)
	if child1.GetStatus() == string(protocol.StatusExited) {
		t.Fatal("child should be alive after spawn")
	}

	// Kill.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID})); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	kcancel()

	// Confirm exited.
	exitedChild := getChild(t, client, childID)
	if exitedChild.GetStatus() != string(protocol.StatusExited) {
		t.Fatalf("want status=exited after kill, got %s", exitedChild.GetStatus())
	}

	// Resume — should re-spawn with the same childId.
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	resp, err := client.Resume(rctx, connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: childID}))
	rcancel()
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if got := resp.Msg.GetChildId(); got != childID {
		t.Errorf("resume: want childId=%s, got %s", childID, got)
	}

	// The resumed child must be alive and, for a kind with a real OS process
	// (claude), have a different PID. An in-process fundi child has PID 0 and
	// never forks, so the "different PID" assertion is only meaningful when the
	// original child had a real PID.
	child2 := getChild(t, client, childID)
	if child2.GetStatus() == string(protocol.StatusExited) {
		t.Fatal("resumed child should be alive, not exited")
	}
	if child1.Pid != nil && child2.Pid != nil && child1.GetPid() != 0 && child1.GetPid() == child2.GetPid() {
		t.Error("resumed child should have a different PID from the original")
	}
}

// TestIntegration_LogDumpOnExit verifies that all four log files are written
// under the daemon's logs dir (paths.LogsDir()/<childId>/) when a child exits
// (Fix 3 / spec §11.3).
func TestIntegration_LogDumpOnExit(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	client := d.control(t)

	childID := d.spawnChild(t)

	// Kill the child to trigger handleChildExit → LogDumper.Dump. Kill returns
	// after process exit, but handleChildExit runs in monitorChild which is
	// concurrent, so the dump files may lag the response slightly.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID})); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	kcancel()

	// Give the daemon a moment to finish the dump. Poll for err.log.gz — the
	// last file Dump writes — to avoid a race where meta.json appears before
	// the gz files are flushed.
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

// TestIntegration_ResumeEmitsSpawned verifies that Resume emits the native
// child_spawned event (Fix 2 / spec §7.2), observed on a global StreamEvents
// subscription.
func TestIntegration_ResumeEmitsSpawned(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	client := d.control(t)

	childID := d.spawnChild(t)

	// Kill the child.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID})); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	kcancel()

	// Set up a global subscription to catch the resumed child's child_spawned
	// event. The daemon publishes it on the RESUMED child's own bus and on the
	// daemon-wide fan-out, both through publishEvent. The watermark read here
	// is what keeps the original spawn's replayed child_spawned from passing
	// the wait below: only an event appended after the open — the resumed
	// child's own — carries a greater ordinal.
	watermark := eventWatermark(t, client, childID)
	es := d.openEvents(t, allEventsSubject(), map[string]int32{childID: watermark - 1}, watermark)

	// Resume.
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Resume(rctx, connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: childID})); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	rcancel()

	// The global subscription must receive child_spawned for the resumed child.
	es.waitEvent(t, func(ev *rafikiv1.Event) bool {
		cs := ev.GetChildSpawned()
		return cs != nil && cs.GetChildId() == childID
	}, 5*time.Second)
}
