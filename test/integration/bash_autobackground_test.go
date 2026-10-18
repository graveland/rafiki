// SPDX-License-Identifier: Apache-2.0

package integration_test

// Auto-background bash, end to end over a real daemon and a real executor.
//
// A fundi child (driven by the hidden --fake-turns scripted sender) runs a
// `bash` call whose command outlives its timeout_ms. The executor's
// auto-background seam promotes the still-running command to a job and returns
// a handoff telling the model so; when the job later exits, the executor pushes
// the exit over WatchJobs and the daemon lands the "finished with exit code"
// fragment in the child's inbox.
//
// Two properties:
//
//   - TestAutoBackgroundLongBashBecomesAJobAndNotifies: the handoff reaches the
//     model (durable tool-result event) and the exit fragment reaches the
//     child's inbox (durable user-message event) within the run.
//
//   - TestAutoBackgroundExitDuringDaemonRestartIsDelivered: the daemon is
//     SIGKILLed before the job exits, so nothing tears the job down and nothing
//     acks its exit; the job finishes against a dead daemon, and the successor
//     daemon delivers the replayed (unacked) exit after restart.
//
// SIGKILL, not SIGTERM, is load-bearing for the restart test: a graceful stop
// runs handleChildExit, which releases the child's workspace, and the executor
// kills and ACKS that workspace's jobs on release (pkg/executor's
// jobRegistry.releaseWorkspace) — so a graceful stop would destroy the very
// unacked exit this test needs.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// autoBgDaemon is a grant daemon (executor listener wired to postgres) whose
// daemon id and TLS material are retained, so a test can SIGKILL it and boot a
// successor against the same home dir, socket, port, certificate and daemon id.
type autoBgDaemon struct {
	*grantDaemon
	daemonID string
	dsn      string
	certPath string
	keyPath  string
}

// bootAutoBgDaemon starts rafikid with an executor listener, mirroring
// bootGrantDaemon but exposing the daemon id and cert paths the restart needs.
func bootAutoBgDaemon(t *testing.T, dsn string) *autoBgDaemon {
	t.Helper()
	c := assert.NewAborting(t)

	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	homeDir, err := os.MkdirTemp(base, "rafiki-autobg")
	c.NoError(err, "mkdirtemp")

	certPath, keyPath, fp := grantCert(t, homeDir)

	appDir := filepath.Join(homeDir, "rafiki")
	socketPath := filepath.Join(appDir, "controller.sock")
	if len(socketPath) > 100 {
		os.RemoveAll(homeDir)
		t.Fatalf("socket path too long: %s", socketPath)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err, "listen")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

	daemonID := nextDaemonID()
	dropDaemonRows(t, daemonID)

	d := startAutoBgDaemon(t, autoBgParams{
		dsn: dsn, homeDir: homeDir, socketPath: socketPath,
		listenAddr: listenAddr, certPath: certPath, keyPath: keyPath, daemonID: daemonID,
	})

	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pool")
	t.Cleanup(pool.Close)

	return &autoBgDaemon{
		grantDaemon: &grantDaemon{
			daemon:      d,
			store:       executorsdb.NewPostgresStore(pool),
			fingerprint: fp,
			listenAddr:  listenAddr,
		},
		daemonID: daemonID,
		dsn:      dsn,
		certPath: certPath,
		keyPath:  keyPath,
	}
}

// autoBgParams is the fixed identity a daemon boot (or its successor) reuses.
type autoBgParams struct {
	dsn        string
	homeDir    string
	socketPath string
	listenAddr string
	certPath   string
	keyPath    string
	daemonID   string
}

// startAutoBgDaemon starts a rafikid with the executor listener wired, waits
// for the control socket to accept, and returns it. It does NOT register a
// home-dir cleanup: the owning test's boot cleanup removes the tree last.
func startAutoBgDaemon(t *testing.T, p autoBgParams) *daemon {
	t.Helper()
	c := assert.NewAborting(t)

	stderr := &stderrBuf{}
	cmd := exec.Command(daemonBinary())
	cmd.Stderr = stderr
	cmd.Env = subprocessEnv(
		"HOME="+p.homeDir,
		"XDG_RUNTIME_DIR="+p.homeDir,
		"XDG_STATE_HOME="+p.homeDir,
		"XDG_DATA_HOME="+p.homeDir,
		"RAFIKI_DB="+p.dsn,
		"RAFIKI_DAEMON_ID="+p.daemonID,
		"RAFIKI_EXECUTORS_ENABLED=1",
		"RAFIKI_CONTROL_LISTEN="+p.listenAddr,
		"RAFIKI_CONTROL_TLS_CERT="+p.certPath,
		"RAFIKI_CONTROL_TLS_KEY="+p.keyPath,
		"RAFIKI_PROXY_LISTEN=127.0.0.1:0",
		// A short debounce so the job-exit fragment reaches the child in well
		// under the tests' waits instead of the 5s production default.
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)
	cmd.Env = append(cmd.Env, noRealProviderEnv()...)
	c.NoError(cmd.Start(), "start daemon")

	d := &daemon{
		socketPath: p.socketPath,
		proc:       cmd,
		homeDir:    p.homeDir,
		logsDir:    filepath.Join(p.homeDir, "rafiki", "logs"),
		stderr:     stderr,
	}
	waitDaemonSocket(t, p.socketPath)
	return d
}

// kill SIGKILLs the running daemon: no handleChildExit, so no workspace
// release and no job teardown, and no ack of the job's exit.
func (h *autoBgDaemon) kill(t *testing.T) {
	t.Helper()
	_ = h.proc.Process.Signal(syscall.SIGKILL)
	_ = h.proc.Wait()
}

// bootSuccessor boots a daemon against the same identity, so the recovery walk
// re-stamps the child row to an id this test's dropDaemonRows sweeps.
func (h *autoBgDaemon) bootSuccessor(t *testing.T) {
	t.Helper()
	d := startAutoBgDaemon(t, autoBgParams{
		dsn: h.dsn, homeDir: h.homeDir, socketPath: h.socketPath,
		listenAddr: h.listenAddr, certPath: h.certPath, keyPath: h.keyPath, daemonID: h.daemonID,
	})
	t.Cleanup(func() {
		_ = d.proc.Process.Signal(syscall.SIGTERM)
		_ = d.proc.Wait()
	})
	h.daemon = d
}

// writeAutoBgTurns writes a --fake-turns script: one bash tool_use call running
// command under timeout_ms, then a plain end_turn reply for the tool result.
func writeAutoBgTurns(t *testing.T, command string, timeoutMs int) string {
	t.Helper()
	c := assert.NewAborting(t)
	commandJSON := mustJSON(t, command)

	toolUse := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":%s,"timeout_ms":%d}}],"usage":{"input_tokens":10,"output_tokens":5}}`, commandJSON, timeoutMs)
	const endTurn = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"ack"}],"usage":{"input_tokens":4,"output_tokens":2}}`

	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(toolUse+"\n"+endTurn+"\n"), 0o600), "write fake-turns script")
	return path
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	assert.NewAborting(t).NoError(err, "marshal %v", v)
	return string(b)
}

// eventUserTexts returns every text the event's user message carries, whether a
// plain text block or the text blocks inside a tool_result block. A nil slice
// means the event is not a user message.
func eventUserTexts(ev *rafikiv1.Event) []string {
	um := ev.GetUserMessage()
	if um == nil {
		return nil
	}
	var out []string
	for _, b := range um.GetContent() {
		if tb := b.GetText(); tb != nil && tb.GetText() != "" {
			out = append(out, tb.GetText())
		}
		if tr := b.GetToolResult(); tr != nil {
			for _, cb := range tr.GetContent() {
				if tb := cb.GetText(); tb != nil && tb.GetText() != "" {
					out = append(out, tb.GetText())
				}
			}
		}
	}
	return out
}

// userTextContains matches a user-message event for childID carrying sub in any
// text it holds.
func userTextContains(childID, sub string) func(*rafikiv1.Event) bool {
	return func(ev *rafikiv1.Event) bool {
		if ev.GetChildId() != childID {
			return false
		}
		for _, text := range eventUserTexts(ev) {
			if strings.Contains(text, sub) {
				return true
			}
		}
		return false
	}
}

// userTextContainsAll matches a user-message event for childID that carries
// every one of subs in a SINGLE text block, so a marker and the fragment that
// names the command must come from the same push.
func userTextContainsAll(childID string, subs ...string) func(*rafikiv1.Event) bool {
	return func(ev *rafikiv1.Event) bool {
		if ev.GetChildId() != childID {
			return false
		}
		for _, text := range eventUserTexts(ev) {
			ok := true
			for _, sub := range subs {
				if !strings.Contains(text, sub) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
}

// openChildEventsAll opens a child-scoped durable stream that replays the
// child's whole log (cursor -1) and treats every event with a non-zero ordinal
// as live, so a wait for a replayed event succeeds whether it was appended
// before or after the stream opened. The child is unique per run, so nothing
// stale for it can exist across runs.
func openChildEventsAll(t *testing.T, g *grantDaemon, childID string) *eventStream {
	t.Helper()
	return g.openEvents(t, childEventsSubject(childID), map[string]int32{childID: -1}, 0)
}

// noEventWithin is the bounded NEGATIVE form of waitEvent: it fails if any live
// buffered event matches predicate at any point within d, and returns otherwise.
// A test that needs an event to be ABSENT before an action has no event to
// subscribe to, so a bounded scan of the buffered stream is the only check —
// this is a test assertion, not a production poll.
func (es *eventStream) noEventWithin(t *testing.T, predicate func(*rafikiv1.Event) bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		es.mu.Lock()
		for _, ev := range es.events {
			if es.isLive(ev) && predicate(ev) {
				dump := es.dumpLocked()
				es.mu.Unlock()
				t.Fatalf("a matching event was present within %v; buffered events:\n%s", d, dump)
			}
		}
		es.mu.Unlock()
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countLiveEventsWithin lets any in-flight events flush for d, then returns how
// many live buffered events match predicate — the "exactly once" check.
func (es *eventStream) countLiveEventsWithin(predicate func(*rafikiv1.Event) bool, d time.Duration) int {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	n := 0
	for _, ev := range es.events {
		if es.isLive(ev) && predicate(ev) {
			n++
		}
	}
	return n
}

// spawnAutoBgChild spawns an executor-bound fundi child driven by the given
// fake-turns script.
func spawnAutoBgChild(t *testing.T, g *grantDaemon, scriptPath string) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := g.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind:             protocol.KindFundi,
		Cwd:              t.TempDir(),
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		ExtraArgs:        []string{"--fake-turns", scriptPath},
	}))
	c.NoError(err, "spawn (executor-bound fundi) failed")
	childID := resp.Msg.GetChildId()
	c.NotEq("", childID, "spawn returned empty childId")
	return childID
}

// promptAutoBgChild sends one prompt frame and waits for it to be accepted.
func promptAutoBgChild(t *testing.T, g *grantDaemon, childID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := g.control(t).SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"go"}`,
	}))
	assert.NewAborting(t).NoError(err, "SendFrame (prompt)")
}

// dropAutoBgInboxRows deletes, on cleanup, the durable inbox rows a test's job
// fragments wrote for childID. dropDaemonRows sweeps conversations.child but
// agent_inbox carries no foreign key to it, so these are otherwise left behind
// in the shared disposable database. Keyed by the per-run-unique child id, so
// it can only ever remove this run's rows.
func dropAutoBgInboxRows(t *testing.T, dsn, childID string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("drop inbox rows for %s: pool: %v", childID, err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.agent_inbox WHERE child_id = $1`, childID); err != nil {
			t.Logf("drop inbox rows for %s: %v", childID, err)
		}
	})
}

// TestAutoBackgroundLongBashBecomesAJobAndNotifies is the end-to-end happy
// path: a long bash call is promoted to a job (the handoff reaches the model)
// and its later exit is pushed into the child's inbox as a fragment.
func TestAutoBackgroundLongBashBecomesAJobAndNotifies(t *testing.T) {
	t.Parallel()
	dsn := requireExecutorDB(t)
	g := bootAutoBgDaemon(t, dsn)
	g.enrollExecutor(t, map[string]string{"env": "home"})
	g.waitForLiveExecutors(t, 1)

	marker := "autobg-" + uniqueSuffix()
	script := writeAutoBgTurns(t, "sleep 3; echo "+marker, 1000)

	childID := spawnAutoBgChild(t, g.grantDaemon, script)
	dropAutoBgInboxRows(t, dsn, childID)
	// Reach a settled point so the child's event log exists before the stream
	// opens and the prompt below is unambiguously a live event.
	waitForStatus(t, g.daemon, childID, "idle", 20*time.Second)
	es := openChildEventsAll(t, g.grantDaemon, childID)

	promptAutoBgChild(t, g.grantDaemon, childID)

	// The handoff reaches the model as the bash tool's result.
	es.waitEvent(t, userTextContains(childID, "moved to the background as job"), 30*time.Second)

	// The job's exit is pushed into the child's inbox: the daemon delivers it as
	// a prompt, which the child echoes as a durable user-message carrying the
	// fragment text. Requiring the run's unique marker in the SAME text pins
	// that this is OUR job's exit, not a stray earlier fragment.
	frag := es.waitEvent(t, userTextContainsAll(childID, "finished with exit code 0", marker), 20*time.Second)
	assert.NewAborting(t).NotNil(frag, "the job-exit fragment never reached the child's inbox")
}

// TestAutoBackgroundExitDuringDaemonRestartIsDelivered pins the no-loss half of
// the push contract across a daemon restart: the job outlives the SIGKILLed
// daemon, its exit is never acked (the daemon was gone when it happened), and
// the successor delivers the replayed exit after recovery.
func TestAutoBackgroundExitDuringDaemonRestartIsDelivered(t *testing.T) {
	t.Parallel()
	dsn := requireExecutorDB(t)
	g := bootAutoBgDaemon(t, dsn)
	g.enrollExecutor(t, map[string]string{"env": "home"})
	g.waitForLiveExecutors(t, 1)

	marker := "autobg-" + uniqueSuffix()
	// The job runs comfortably longer than the ~1s handoff below, so the exit
	// cannot happen before the SIGKILL even on a stalled host: the delivery
	// after restart is a genuine REPLAY, never a live push the old daemon made
	// and the first stream happened to miss.
	script := writeAutoBgTurns(t, "sleep 8; echo "+marker, 1000)

	childID := spawnAutoBgChild(t, g.grantDaemon, script)
	dropAutoBgInboxRows(t, dsn, childID)
	waitForStatus(t, g.daemon, childID, "idle", 20*time.Second)
	es := openChildEventsAll(t, g.grantDaemon, childID)

	promptAutoBgChild(t, g.grantDaemon, childID)
	// Barrier: the handoff proves the command was promoted to a running job
	// BEFORE the daemon is killed, so the restart test cannot pass by the call
	// never having started. The job minimum (sleep 8, promoted at ~1s) then
	// outlives the kill below with room to spare.
	es.waitEvent(t, userTextContains(childID, "moved to the background as job"), 30*time.Second)

	// The exit fragment must NOT have reached the first daemon's stream before
	// the kill: if it had, the post-restart delivery could be that same live
	// push replayed from the log rather than the executor's unacked replay, and
	// the test would pass without exercising the no-loss property at all.
	es.noEventWithin(t, userTextContainsAll(childID, "finished with exit code 0", marker), time.Second)

	// SIGKILL the daemon before the job (sleep 8, promoted at ~1s) can exit:
	// no handleChildExit, no workspace release, no job teardown, and no ack,
	// so the executor keeps the exit unacked.
	g.kill(t)

	// Let the job finish while the daemon is down: this is what makes the
	// delivery below a REPLAY of an unacked exit rather than a live push. A
	// bounded wait for an external process — the executor's job is unreachable
	// from here, so there is no event to subscribe to, only its own 8s runtime.
	time.Sleep(10 * time.Second)

	// The successor recovers the child, the executor reconnects and replays the
	// unacked exit, and the daemon hands the fragment to the event buffer.
	g.bootSuccessor(t)
	waitForStatus(t, g.daemon, childID, "idle", 60*time.Second)
	es2 := openChildEventsAll(t, g.grantDaemon, childID)
	frag := es2.waitEvent(t, userTextContainsAll(childID, "finished with exit code 0", marker), 30*time.Second)
	assert.NewAborting(t).NotNil(frag, "the replayed job-exit fragment never reached the child's inbox after restart")

	// EXACTLY once: an exit acked after delivery must not be replayed again on
	// the next subscribe. The window is far shorter than the resumed child's
	// own new job (sleep 8), so a second occurrence can only be a duplicate of
	// THIS exit.
	mark := userTextContainsAll(childID, "finished with exit code 0", marker)
	ck := assert.NewAborting(t)
	ck.Eq(1, es2.countLiveEventsWithin(mark, time.Second),
		"the replayed exit fragment must arrive exactly once")
}

// enrollExecutorUDS is enrollExecutor with a native `rafiki executor serve`
// that reverse-dials the daemon's EXECUTOR UNIX SOCKET instead of the TLS
// listener. The unix listener accepts connections BEFORE the recovery walk
// (cmd/rafikid/main.go), so this is the transport on which an executor can
// reconnect before loadChildren has populated its child's owner — the window a
// TLS-connected executor cannot reach, because that listener starts only after
// recovery.
func (g *autoBgDaemon) enrollExecutorUDS(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)

	marker := "t" + uniqueSuffix()
	token, err := g.store.MintToken(context.Background(), executors.NewToken{
		Labels:        map[string]string{"env": "home", "test-run": marker},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint token")

	execSock := filepath.Join(g.homeDir, "rafiki", "executor.sock")
	execHome := t.TempDir()
	credFile := filepath.Join(t.TempDir(), "cred")
	root := t.TempDir()
	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect-socket", execSock,
		"--enroll-token", token,
		"--credential-file", credFile,
		"--root", root,
	)
	cmd.Env = subprocessEnv(
		"HOME="+execHome,
		"XDG_RUNTIME_DIR="+execHome,
		"XDG_STATE_HOME="+execHome,
		"XDG_DATA_HOME="+execHome,
		"XDG_CONFIG_HOME="+execHome,
	)
	c.NoError(cmd.Start(), "start executor (uds)")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	// Wait for the executor to enroll and go live — the same marker discipline as
	// grantDaemon.enrollExecutor, so a stale row of a previous run is never
	// mistaken for this one.
	deadline := time.Now().Add(10 * time.Second)
	var enrolledID string
	for time.Now().Before(deadline) {
		execs, _ := g.store.List(context.Background())
		for _, e := range execs {
			if e.Labels["test-run"] == marker && !e.LastSeenAt.IsZero() {
				enrolledID = e.ID
				break
			}
		}
		if enrolledID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.NotEq("", enrolledID, "executor (uds) never enrolled and became live")
	return enrolledID
}

// TestAutoBackgroundExitDuringDaemonRestartOverUnixSocketIsDelivered is
// TestAutoBackgroundExitDuringDaemonRestartIsDelivered with the executor on the
// daemon's executor UNIX socket. The unix listener starts BEFORE loadChildren,
// so an executor that reconnects over it can open WatchJobs before recovery has
// populated the owner; the pool holds that first open until recovery completes,
// so the replayed unacked exit is still delivered rather than dropped-and-acked.
func TestAutoBackgroundExitDuringDaemonRestartOverUnixSocketIsDelivered(t *testing.T) {
	t.Parallel()
	dsn := requireExecutorDB(t)
	g := bootAutoBgDaemon(t, dsn)
	g.enrollExecutorUDS(t)
	g.waitForLiveExecutors(t, 1)

	marker := "autobg-" + uniqueSuffix()
	script := writeAutoBgTurns(t, "sleep 8; echo "+marker, 1000)

	childID := spawnAutoBgChild(t, g.grantDaemon, script)
	dropAutoBgInboxRows(t, dsn, childID)
	waitForStatus(t, g.daemon, childID, "idle", 20*time.Second)
	es := openChildEventsAll(t, g.grantDaemon, childID)

	promptAutoBgChild(t, g.grantDaemon, childID)
	es.waitEvent(t, userTextContains(childID, "moved to the background as job"), 30*time.Second)
	// The fragment must not have been delivered live before the kill; see the
	// TLS variant's comment for why this guards the no-loss claim.
	es.noEventWithin(t, userTextContainsAll(childID, "finished with exit code 0", marker), time.Second)

	g.kill(t)
	// The job (sleep 8, promoted at ~1s) finishes while the daemon is down.
	time.Sleep(10 * time.Second)

	g.bootSuccessor(t)
	waitForStatus(t, g.daemon, childID, "idle", 60*time.Second)
	es2 := openChildEventsAll(t, g.grantDaemon, childID)
	frag := es2.waitEvent(t, userTextContainsAll(childID, "finished with exit code 0", marker), 30*time.Second)
	assert.NewAborting(t).NotNil(frag, "the replayed job-exit fragment never reached the child's inbox after a unix-socket restart")

	mark := userTextContainsAll(childID, "finished with exit code 0", marker)
	assert.NewAborting(t).Eq(1, es2.countLiveEventsWithin(mark, time.Second),
		"the replayed exit fragment must arrive exactly once over the unix socket")
}
