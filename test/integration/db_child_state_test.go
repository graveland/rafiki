package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

// bootDaemonDB is bootDaemon with a RAFIKI_DB and RAFIKI_DAEMON_ID.
func bootDaemonDB(t *testing.T, daemonID string) *daemon {
	t.Helper()

	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	// Ensure migrations are applied.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()

	dropDaemonRows(t, daemonID)

	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	homeDir, err := os.MkdirTemp(base, "rafiki-it-db")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}

	appDir := filepath.Join(homeDir, "rafiki")
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
		"RAFIKI_DB="+dsn,
		"RAFIKI_DAEMON_ID="+daemonID,
	)

	if err := cmd.Start(); err != nil {
		os.RemoveAll(homeDir)
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemon{
		socketPath: socketPath,
		proc:       cmd,
		homeDir:    homeDir,
		logsDir:    filepath.Join(appDir, "logs"),
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(d.stopDaemonNoRemove)
	// Don't auto-remove homeDir so we can restart the daemon against the same dir.
	return d
}

func (d *daemon) stopDaemonNoRemove() {
	if d.proc != nil && d.proc.Process != nil {
		_ = d.proc.Process.Signal(syscall.SIGTERM)
		_ = d.proc.Wait()
	}
}

// TestDBChildState_RestartSurvivesWipedStateDir is the core acceptance test.
// It spawns a fundi child, stops the daemon, wipes the state directory, restarts
// with the same daemon id, and verifies the child is still present.
func TestDBChildState_RestartSurvivesWipedStateDir(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	daemonID := "test-daemon-1"

	// 1. Start daemon, spawn a fundi child.
	d1 := bootDaemonDB(t, daemonID)
	childID := d1.spawnChild(t)

	// Send a message to create a conversation.
	sendJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"s1","childId":%q,"frame":{"type":"get_state","id":"u1"}}`,
		childID,
	)
	raw := d1.request(t, sendJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send failed: %+v", r.Error)
	}

	// 2. Stop the daemon.
	_ = d1.proc.Process.Signal(syscall.SIGTERM)
	_ = d1.proc.Wait()

	// 3. Wipe the state directory.
	stateDir := filepath.Join(d1.homeDir, "rafiki", "state")
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("remove state dir: %v", err)
	}

	// 4. Restart with the same daemon id and home dir.
	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(),
		"HOME="+d1.homeDir,
		"XDG_RUNTIME_DIR="+d1.homeDir,
		"XDG_STATE_HOME="+d1.homeDir,
		"XDG_DATA_HOME="+d1.homeDir,
		"RAFIKI_DB="+os.Getenv("RAFIKI_TEST_DSN"),
		"RAFIKI_DAEMON_ID="+daemonID,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start second daemon: %v", err)
	}
	d2 := &daemon{
		socketPath: d1.socketPath,
		proc:       cmd,
		homeDir:    d1.homeDir,
		logsDir:    d1.logsDir,
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", d2.socketPath)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = d2.proc.Process.Signal(syscall.SIGTERM)
		_ = d2.proc.Wait()
		os.RemoveAll(d2.homeDir)
	})

	// 5. Assert the child is still listed.
	raw = d2.request(t, `{"type":"ctrl_list","id":"l1"}`)
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
		t.Fatal("child not found after restart with wiped state dir")
	}

	// 6. Clean up: kill, then forget.
	killAndForget(t, d2, childID)
}

// TestDBChildState_ChildRowVisibleAfterRestart verifies that a child row
// inserted directly into conversations.child is loaded by the daemon on
// restart — it appears in ctrl_list as exited.
func TestDBChildState_ChildRowVisibleAfterRestart(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	dsn := os.Getenv("RAFIKI_TEST_DSN")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	convID := insertTestConversation(t, pool)

	childID := fmt.Sprintf("c_%s", time.Now().Format("150405.000000"))
	configJSON, _ := json.Marshal(childstore.ChildConfig{})
	labelsJSON, _ := json.Marshal(map[string]string{})
	_, err = pool.Exec(context.Background(), `
		INSERT INTO conversations.child
		    (child_id, kind, status, last_status, spawned_at, workspace_mode, conversation_id, config, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8, $9)`,
		childID, protocol.KindFundi, string(protocol.StatusExited), "idle",
		time.Now(), "ephemeral", convID, configJSON, labelsJSON)
	if err != nil {
		t.Fatalf("insert child row: %v", err)
	}
	// A fresh pool: the one above is closed by this function's defer, which
	// runs BEFORE cleanups, so reusing it deletes nothing and — because the
	// error is discarded — says nothing about it either. The row carries no
	// daemon_id (it was written here, not by a daemon), so dropDaemonRows
	// cannot sweep it.
	t.Cleanup(func() {
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("drop child row %s: pool: %v", childID, err)
			return
		}
		defer p.Close()
		if _, err := p.Exec(context.Background(),
			`DELETE FROM conversations.child WHERE child_id = $1`, childID); err != nil {
			t.Logf("drop child row %s: %v", childID, err)
		}
	})

	d := bootDaemonDB(t, "test-visible-daemon")

	raw := d.request(t, `{"type":"ctrl_list","id":"l1"}`)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	var listData protocol.ListResponseData
	mustUnmarshal(t, r.Data, &listData)

	found := false
	for _, c := range listData.Children {
		if c.ChildID == childID {
			found = true
			if c.Status != string(protocol.StatusExited) {
				t.Errorf("recovered child status = %q, want %q", c.Status, protocol.StatusExited)
			}
			break
		}
	}
	if !found {
		t.Error("child row not found in ctrl_list — recovery did not load it")
	}

	d.stopDaemonNoRemove()
	os.RemoveAll(d.homeDir)
}

func insertTestConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

// ─── helpers for crash/recovery tests ─────────────────────────────────────────

// sendTurn sends a ctrl_send with a get_state frame, which creates a conversation
// for the child without needing a real model turn.
func sendTurn(t *testing.T, d *daemon, childID string) {
	t.Helper()
	sendJSON := fmt.Sprintf(
		`{"type":"ctrl_send","id":"s1","childId":%q,"frame":{"type":"get_state","id":"u1"}}`,
		childID,
	)
	raw := d.request(t, sendJSON)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send failed: %+v", r.Error)
	}
}

// killAndForget stops a recovered child and forgets it, tolerating the race
// against recovery's auto-resume.
//
// A child whose last_status says it was alive when the daemon stopped is
// auto-resumed by loadChildren, on a goroutine, so a test that has only waited
// for the control socket can arrive at any point in that sequence: the child
// may still be exited (kill answers child_exited), may be mid-resume, or may
// already be running (forget answers not_exited). Neither verb alone is
// therefore conclusive, and asserting on one reading of a state that is still
// moving is what made this cleanup flaky. Retrying the pair until forget
// succeeds converges on the one state both agree about.
func killAndForget(t *testing.T, d *daemon, childID string) {
	t.Helper()

	killJSON := fmt.Sprintf(`{"type":"ctrl_kill","id":"k1","childId":%q}`, childID)
	forgetJSON := fmt.Sprintf(`{"type":"ctrl_forget","id":"f1","childId":%q}`, childID)

	var last *protocol.ErrorBody
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var r protocol.Response

		// ctrl_kill returns only once the child reads as exited (Kill waits
		// for cm.Remove), so a success here needs no poll before the forget.
		// child_exited just means the resume had not landed yet.
		mustUnmarshal(t, d.request(t, killJSON), &r)
		if !r.Success && r.Error != nil && r.Error.Code != protocol.ErrChildExited {
			t.Fatalf("ctrl_kill failed: %+v", r.Error)
		}

		mustUnmarshal(t, d.request(t, forgetJSON), &r)
		if r.Success {
			return
		}
		last = r.Error
		if r.Error != nil && r.Error.Code != protocol.ErrNotExited {
			t.Fatalf("ctrl_forget failed: %+v", r.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("ctrl_forget never succeeded; last error: %+v", last)
}

// itDaemonSeq numbers the daemons this suite boots.
//
// RAFIKI_DAEMON_ID must be unique per RUNNING daemon: LeaseStore.Acquire lets a
// daemon reclaim its own leases by id, so two live daemons sharing one would
// reclaim each other's and reproduce the split brain the lease exists to
// prevent. Several tests boot more than one daemon, so the id cannot be
// derived from the test name alone.
var itDaemonSeq atomic.Int64

func nextDaemonID() string {
	return fmt.Sprintf("it-%d-%d", os.Getpid(), itDaemonSeq.Add(1))
}

// dropDaemonRows deletes, on cleanup, the child rows a test daemon wrote.
//
// The whole suite shares one database, and a daemon recovers EVERY row in
// conversations.child at startup regardless of which daemon wrote it — child
// rows are shared by design. So a child left behind is resurrected and
// auto-resumed by every subsequent daemon boot, in this run and in every run
// after it. Unchecked, that residue compounds until each boot is resuming
// dozens of dead fundi engines; at around forty it made the recovery tests
// fail outright. Deleting by daemon_id keeps the sweep to rows this test's
// own daemon created.
func dropDaemonRows(t *testing.T, daemonID string) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" || daemonID == "" {
		return
	}
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("drop rows for daemon %s: pool: %v", daemonID, err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.child WHERE daemon_id = $1`, daemonID); err != nil {
			t.Logf("drop rows for daemon %s: %v", daemonID, err)
		}
	})
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return pool
}

// ─── crash recovery test ────────────────────────────────────────────────────

// TestDBChildState_ResumesAfterDaemonCrash is the regression test for the
// defect that broke the design's motivating scenario.
func TestDBChildState_ResumesAfterDaemonCrash(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	daemonID := "daemon-crash"

	// Ensure migrations.
	{
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		if err := store.Migrate(context.Background(), pool); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		pool.Close()
	}

	d1 := bootDaemonDB(t, daemonID)
	childID := d1.spawnChild(t)
	sendTurn(t, d1, childID)

	// SIGKILL — handleChildExit never runs.
	_ = d1.proc.Process.Signal(syscall.SIGKILL)
	_ = d1.proc.Wait()

	// Verify last_status is NULL (the test is exercising a crash).
	pool := openPool(t, dsn)
	defer pool.Close()
	var lastStatus *string
	if err := pool.QueryRow(context.Background(),
		`SELECT last_status FROM conversations.child WHERE child_id = $1`, childID).Scan(&lastStatus); err != nil {
		t.Fatalf("read last_status: %v", err)
	}
	if lastStatus != nil && *lastStatus != "" {
		t.Fatalf("last_status = %q; the test is not exercising a crash", *lastStatus)
	}

	// Restart with the same daemon id and home dir.
	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(),
		"HOME="+d1.homeDir,
		"XDG_RUNTIME_DIR="+d1.homeDir,
		"XDG_STATE_HOME="+d1.homeDir,
		"XDG_DATA_HOME="+d1.homeDir,
		"RAFIKI_DB="+dsn,
		"RAFIKI_DAEMON_ID="+daemonID,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start second daemon: %v", err)
	}
	d2 := &daemon{socketPath: d1.socketPath, proc: cmd, homeDir: d1.homeDir}
	t.Cleanup(func() {
		_ = d2.proc.Process.Signal(syscall.SIGTERM)
		_ = d2.proc.Wait()
		os.RemoveAll(d2.homeDir)
	})

	dl := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl) {
		conn, err := net.Dial("unix", d2.socketPath)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Check the child is listed — that proves recovery ran.
	raw := d2.request(t, `{"type":"ctrl_list","id":"l1"}`)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	var listData protocol.ListResponseData
	mustUnmarshal(t, r.Data, &listData)
	for _, c := range listData.Children {
		if c.ChildID == childID {
			return // found
		}
	}
	t.Fatal("child not found after crash recovery")
}
