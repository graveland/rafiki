// SPDX-License-Identifier: Apache-2.0

package integration_test

// Fundi restart-resume, end to end on the real daemon: the auto-resume a
// daemon restart performs, and whether it re-submits the interrupted turn.
//
// Two pinned behaviors:
//
//   - TestDBChildState_AutoResumeReissuesMidTurnAfterCrash: a fundi child
//     blocked inside its LLM call is SIGKILLed along with its daemon; the
//     successor daemon auto-resumes it and the engine's startupResume
//     (agentloop.Resume) re-issues the turn — exactly one extra LLM request,
//     one user row and one assistant row, no duplicates.
//
//   - TestDBChildState_AutoResumeIdleAfterGracefulRestart: an idle child
//     survives a graceful SIGTERM (rows keep their live status; the shutdown
//     writes only shutting_down) and the successor auto-resumes it to idle
//     WITHOUT issuing any LLM request.
//
// The fake provider answers a real SSE stream: a streaming Messages request
// answered with a plain JSON body parses as an empty message with no error
// (the ssestream decoder skips non-data lines and reports none), which reads
// downstream as "unexpected stop reason" and fatals the turn — so any fake
// seat for these tests must speak SSE.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// blockingFakeLLM answers /v1/messages with a canned SSE end_turn reply. It
// blocks every request until `release` is closed, so a fundi child that called
// it stays mid-turn (status streaming) for as long as the test needs. Requests
// are counted: the re-issue assertions below read the count.
type blockingFakeLLM struct {
	srv      *httptest.Server
	release  chan struct{}
	requests atomic.Int64
}

func newBlockingFakeLLM(t *testing.T) *blockingFakeLLM {
	t.Helper()
	f := &blockingFakeLLM{release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.requests.Add(1)
		select {
		case <-f.release:
		case <-time.After(90 * time.Second):
			// keep the test bounded; answer anyway
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_fake_1","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}` + "\n\n",
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		} {
			_, _ = w.Write([]byte(ev))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeFakeSSEProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.fakellm]
kind = "anthropic"
base_url = %q
`, baseURL)
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write providers fixture")
	return path
}

// restartResumeDaemon boots a successor daemon against the same home dir,
// socket, daemon id and env, and waits for the control socket to accept.
func restartResumeDaemon(t *testing.T, d1 *daemon, id, providersPath string, extraEnv ...string) *daemon {
	t.Helper()
	cmd := exec.Command(daemonBinary())
	cmd.Env = append(os.Environ(),
		"HOME="+d1.homeDir,
		"XDG_RUNTIME_DIR="+d1.homeDir,
		"XDG_STATE_HOME="+d1.homeDir,
		"XDG_DATA_HOME="+d1.homeDir,
		"RAFIKI_DB="+os.Getenv("RAFIKI_TEST_DSN"),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+providersPath,
		"RAFIKI_PROXY_LISTEN=127.0.0.1:0",
		"RAFIKI_DAEMON_ID="+id,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	assert.NewAborting(t).NoError(cmd.Start(), "start successor daemon")
	d2 := &daemon{socketPath: d1.socketPath, proc: cmd, homeDir: d1.homeDir, logsDir: d1.logsDir, stderr: stderr}
	t.Cleanup(func() {
		_ = d2.proc.Process.Signal(syscall.SIGTERM)
		_ = d2.proc.Wait()
		os.RemoveAll(d2.homeDir)
	})
	deadline := time.Now().Add(15 * time.Second)
	var accepted bool
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", d2.socketPath)
		if err == nil {
			_ = conn.Close()
			accepted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !accepted {
		t.Fatalf("successor daemon never accepted on %s\nstderr:\n%s", d2.socketPath, stderr.tail(8000))
	}
	return d2
}

// waitForStatus polls GetChild until the status matches, else fails.
func waitForStatus(t *testing.T, d *daemon, childID, want string, timeout time.Duration) *rafikiv1.ChildSummary {
	t.Helper()
	var last *rafikiv1.ChildSummary
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range listChildren(t, d.control(t)) {
			if c.GetChildId() == childID {
				last = c
				if c.GetStatus() == want {
					return c
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("child %s never reached status %q (last %q)", childID, want, last.GetStatus())
	}
	t.Fatalf("child %s not found while waiting for %q", childID, want)
	return nil
}

// waitForStatusNoFail polls until the status matches or the timeout elapses,
// returning the last seen summary (nil when never listed).
func waitForStatusNoFail(t *testing.T, d *daemon, childID, want string, timeout time.Duration) *rafikiv1.ChildSummary {
	t.Helper()
	var last *rafikiv1.ChildSummary
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range listChildren(t, d.control(t)) {
			if c.GetChildId() == childID {
				last = c
				if c.GetStatus() == want {
					return c
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

type convRow struct {
	role    string
	text    string
	ordinal int
}

func readResumeConversation(t *testing.T, dsn, conversationID string) []convRow {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	rows, err := pool.Query(context.Background(), `
		SELECT role::text, content::text, ordinal
		FROM conversations.conversation_message WHERE conversation_id = $1::uuid ORDER BY ordinal`, conversationID)
	assert.NewAborting(t).NoError(err, "query messages")
	defer rows.Close()
	var out []convRow
	for rows.Next() {
		var r convRow
		var content string
		assert.NewAborting(t).NoError(rows.Scan(&r.role, &content, &r.ordinal), "scan message")
		r.text = content
		out = append(out, r)
	}
	return out
}

func resumeConversationID(t *testing.T, dsn, childID string) string {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	var id string
	err := pool.QueryRow(context.Background(), `
		SELECT c.id::text FROM conversations.conversation c
		JOIN conversations.child ch ON ch.conversation_id = c.id
		WHERE ch.child_id = $1`, childID).Scan(&id)
	assert.NewAborting(t).NoError(err, "read conversation id for %s", childID)
	return id
}

// TestScratchRestartResumesMidTurn: fundi child mid-LLM-call, daemon SIGKILLed,
// successor boots. Expect: auto-resume, and the successor's engine re-issues
// the turn — the fake server sees a SECOND request.
func TestDBChildState_AutoResumeReissuesMidTurnAfterCrash(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	fake := newBlockingFakeLLM(t)
	providers := writeFakeSSEProviders(t, fake.srv.URL)
	id := nextDaemonID()
	extra := append(noRealProviderEnv(),
		"RAFIKI_PROVIDERS="+providers,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)
	d1 := bootDaemonDB(t, id, extra...)

	childID := spawnResumeChild(t, d1, "fakellm/mini")
	waitForStatus(t, d1, childID, "idle", 20*time.Second)

	// Send a prompt; the fake blocks it, so the child goes and stays streaming.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := d1.control(t).SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","id":"u1","message":"do work"}`,
	}))
	ck.NoError(err, "SendFrame prompt")
	waitForStatus(t, d1, childID, string(protocol.StatusStreaming), 20*time.Second)

	// SIGKILL mid-turn.
	_ = d1.proc.Process.Signal(syscall.SIGKILL)
	_ = d1.proc.Wait()
	t.Logf("fake saw %d request(s) before kill", fake.requests.Load())

	// Successor boots.
	d2 := restartResumeDaemon(t, d1, id, providers, extra...)

	// The auto-resume should re-issue the turn: status streaming again.
	waitForStatus(t, d2, childID, string(protocol.StatusStreaming), 30*time.Second)
	t.Logf("fake requests right after restart: %d", fake.requests.Load())

	// Release; the re-issued turn completes.
	close(fake.release)
	final := waitForStatusNoFail(t, d2, childID, "idle", 30*time.Second)
	total := fake.requests.Load()
	t.Logf("fake total requests: %d (want 2: pre-kill + re-issued)", total)
	if final == nil {
		t.Fatalf("child vanished from list after restart")
	}
	if final.GetStatus() != "idle" {
		t.Logf("FINAL STATUS %q exit=%v sig=%v", final.GetStatus(), final.GetExitCode(), final.GetExitSignal())
		t.Logf("successor stderr tail:\n%s", d2.stderr.tail(12000))
		t.Fatalf("child ended %q instead of idle", final.GetStatus())
	}

	// The conversation must carry exactly ONE user row and ONE assistant row.
	convID := resumeConversationID(t, dsn, childID)
	rows := readResumeConversation(t, dsn, convID)
	for _, r := range rows {
		t.Logf("row ord=%d role=%s content=%s", r.ordinal, r.role, truncateForLog(r.text, 120))
	}
	var users, assistants int
	for _, r := range rows {
		if r.role == "user" {
			users++
		}
		if r.role == "assistant" {
			assistants++
		}
	}
	if users != 1 || assistants != 1 {
		t.Fatalf("conversation rows: %d user / %d assistant (want 1/1)", users, assistants)
	}
}

// TestScratchRestartResumesIdle: fundi child idle at graceful restart. Expect
// auto-resume to idle, and NO extra LLM request.
func TestDBChildState_AutoResumeIdleAfterGracefulRestart(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	fake := newBlockingFakeLLM(t)
	close(fake.release) // never blocks
	providers := writeFakeSSEProviders(t, fake.srv.URL)
	id := nextDaemonID()
	extra := append(noRealProviderEnv(),
		"RAFIKI_PROVIDERS="+providers,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)
	d1 := bootDaemonDB(t, id, extra...)

	childID := spawnResumeChild(t, d1, "fakellm/mini")
	waitForStatus(t, d1, childID, "idle", 20*time.Second)

	// One complete turn.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := d1.control(t).SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","id":"u1","message":"say hi"}`,
	}))
	ck.NoError(err, "SendFrame prompt")
	// The turn runs asynchronously; wait for the fake to see it complete.
	deadline := time.Now().Add(30 * time.Second)
	for fake.requests.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	before := fake.requests.Load()
	if before != 1 {
		t.Logf("d1 stderr tail:\n%s", d1.stderr.tail(6000))
		t.Fatalf("expected 1 completed turn before restart, saw %d", before)
	}
	final1 := waitForStatusNoFail(t, d1, childID, "idle", 30*time.Second)
	if final1 == nil || final1.GetStatus() != "idle" {
		t.Logf("d1 stderr tail:\n%s", d1.stderr.tail(6000))
		t.Fatalf("pre-restart child ended %v instead of idle", final1)
	}

	// Graceful SIGTERM; rows keep live statuses (idle).
	_ = d1.proc.Process.Signal(syscall.SIGTERM)
	_ = d1.proc.Wait()

	d2 := restartResumeDaemon(t, d1, id, providers, extra...)
	final2 := waitForStatusNoFail(t, d2, childID, "idle", 30*time.Second)
	if final2 == nil || final2.GetStatus() != "idle" {
		t.Logf("d2 stderr:\n%s", d2.stderr.tail(12000))
		t.Fatalf("post-restart child ended %v instead of idle", final2)
	}
	// Wait a moment to catch a spurious turn.
	time.Sleep(2 * time.Second)
	after := fake.requests.Load()
	if after != 1 {
		t.Fatalf("auto-resume of an idle child issued %d extra LLM request(s) (want 0)", after-1)
	}
}

func spawnResumeChild(t *testing.T, d *daemon, model string) string {
	t.Helper()
	ck := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:            "/tmp",
		Kind:           protocol.KindFundi,
		Model:          model,
		RecordRequests: true,
	}))
	ck.NoError(err, "spawn failed")
	ck.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─── claude restart auto-resume ──────────────────────────────────────────────

// insertScratchClaudeRow hand-inserts a claude child row, so the test can
// control the status and session id the recovery walk sees (a spawned fake
// claude never reports a session: it is silent by design). daemon_id carries
// the booting daemon's id so dropDaemonRows sweeps it on cleanup.
func insertScratchClaudeRow(t *testing.T, dsn, daemonID, childID, status string) {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	configJSON, _ := json.Marshal(childstore.ChildConfig{})
	labelsJSON, _ := json.Marshal(map[string]string{
		"rafiki/daemon": daemonID,
		"owner":         "brent",
	})
	_, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.child
		    (child_id, kind, status, spawned_at, model, cwd, config_dir,
		     session_id, config, labels, daemon_id)
		VALUES ($1, $2, $3, now(), $4, '/tmp', '',
		        $5, $6, $7, $8)`,
		childID, "claude", status, "anthropic/sonnet-latest",
		"sess-"+childID, configJSON, labelsJSON, daemonID)
	assert.NewAborting(t).NoError(err, "insert claude child row %s", childID)
}

// claudeStdin reads the stdin half of a fake-claude dump: everything after the
// ---STDIN--- marker, i.e. exactly what the daemon wrote to the child.
func claudeStdin(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	assert.NewAborting(t).NoError(err, "read dump %s", path)
	const marker = "---STDIN---\n"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		return ""
	}
	return string(raw[i+len(marker):])
}

// TestDBChildState_AutoResumesClaudeChildren pins the claude half of restart
// recovery: rows that were working are relaunched WITH their session
// (--resume) and get the restart continuation prompt through the durable
// inbox; rows that were idle are relaunched with no prompt at all.
//
// The rows are hand-inserted (a spawned fake claude is silent, so it never
// reports a session id for --resume to carry), with the fixture recording both
// argv and stdin — the argv proves the relaunch re-attaches the session, the
// stdin proves the nudge reaches exactly the child that was mid-turn.
func TestDBChildState_AutoResumesClaudeChildren(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	dumps := t.TempDir()
	id := nextDaemonID()
	extra := append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"CLAUDE_BINARY="+fakeClaudeBin(t),
		"FAKE_CLAUDE_DIR="+dumps,
	)
	d1 := bootDaemonDB(t, id, extra...)

	const workingID = "c_claude_working"
	const idleID = "c_claude_idle"
	insertScratchClaudeRow(t, dsn, id, workingID, "streaming")
	insertScratchClaudeRow(t, dsn, id, idleID, "idle")

	// Crash the daemon; the successor recovers both rows.
	_ = d1.proc.Process.Signal(syscall.SIGKILL)
	_ = d1.proc.Wait()
	d2 := restartResumeDaemon(t, d1, id, "", extra...)

	// Both children must be live again.
	finalW := waitForStatusNoFail(t, d2, workingID, "idle", 60*time.Second)
	if finalW == nil || finalW.GetStatus() != "idle" {
		t.Logf("successor stderr:\n%s", d2.stderr.tail(14000))
		t.Fatalf("working claude child ended %v instead of idle", finalW)
	}
	waitForStatus(t, d2, idleID, "idle", 60*time.Second)

	// The relaunch re-attaches the stored session.
	dworking := waitClaudeDump(t, d2, dumps, workingID)
	ck.Contains(dworking.argv, "--resume", "the relaunched claude argv")
	ck.Contains(dworking.argv, "sess-"+workingID, "the relaunched claude re-attaches its session")
	didle := waitClaudeDump(t, d2, dumps, idleID)
	ck.Contains(didle.argv, "--resume", "the idle child is relaunched with its session too")

	// The continuation prompt reaches ONLY the child that was mid-turn. Both
	// dumps come from the successor's launches; find each dump's own file to
	// read its stdin half.
	stdinOf := func(childID string) string {
		matches, _ := filepath.Glob(filepath.Join(dumps, "claude-*.dump"))
		for _, p := range matches {
			d, ok := parseClaudeDump(t, p)
			if ok && d.envValue(paths.ChildID) == childID {
				return claudeStdin(t, p)
			}
		}
		t.Fatalf("no dump file for %s", childID)
		return ""
	}
	nudged := stdinOf(workingID)
	ck.StrContains(nudged, "daemon restarted while your previous turn was in flight",
		"the working child's stdin carries the continuation prompt; got:\n%s", nudged)
	ck.Empty(strings.TrimSpace(stdinOf(idleID)),
		"the idle child's relaunch must send no prompt")
}
