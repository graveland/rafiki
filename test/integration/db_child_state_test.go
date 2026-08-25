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
		"RAFIKI_PI_BINARY="+fakePiPath,
		"RAFIKI_DB="+dsn,
		"RAFIKI_DAEMON_ID="+daemonID,
	)
	// cmd.Stderr = os.Stderr

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
		"RAFIKI_PI_BINARY="+fakePiPath,
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

	// 6. Clean up: forget the child.
	forgetJSON := fmt.Sprintf(`{"type":"ctrl_forget","id":"f1","childId":%q}`, childID)
	raw = d2.request(t, forgetJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Errorf("ctrl_forget failed: %+v", r.Error)
	}
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
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM conversations.child WHERE child_id = $1`, childID)
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

// waitForConversationID polls the child row until a conversation_id is written.
func waitForConversationID(t *testing.T, pool *pgxpool.Pool, childID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(conversation_id::text, '') FROM conversations.child
			  WHERE child_id = $1`, childID).Scan(&id); err == nil && id != "" {
			return id
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("child %s never got a conversation_id", childID)
	return ""
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
		"RAFIKI_PI_BINARY="+fakePiPath,
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
