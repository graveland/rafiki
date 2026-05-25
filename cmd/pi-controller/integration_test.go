package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/server"
	"graveland.dev/pi-controller/internal/store"
)

// testSocketDir returns a temp directory with a short path (macOS UDS paths
// are capped at 104 bytes; t.TempDir() names exceed this for long test names).
func testSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pic")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestIntegration_CtrlStatus boots the controller in-process, connects via
// UDS, sends a ctrl_status frame, and asserts the response is a success.
func TestIntegration_CtrlStatus(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := store.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath)

	handler := server.NewDispatch(ctrl)
	srv, err := server.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	frame := `{"type":"ctrl_status","id":"integration-1"}` + "\n"
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var resp protocol.Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nframe: %s", err, line)
	}
	if !resp.Success {
		t.Fatalf("expected success, got error=%+v", resp.Error)
	}
	if resp.Command != protocol.TypeCtrlStatus {
		t.Errorf("command: want %s, got %s", protocol.TypeCtrlStatus, resp.Command)
	}
	if resp.ID != "integration-1" {
		t.Errorf("id: want integration-1, got %s", resp.ID)
	}

	var data protocol.StatusResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Version != version {
		t.Errorf("version: want %s, got %s", version, data.Version)
	}
}

// TestIntegration_CtrlList boots the controller, sends ctrl_list, verifies
// the response contains an empty children array (no children have been spawned).
func TestIntegration_CtrlList(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := store.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath)

	handler := server.NewDispatch(ctrl)
	srv, err := server.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte(`{"type":"ctrl_list","id":"list-1"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var resp protocol.Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nframe: %s", err, line)
	}
	if !resp.Success {
		t.Fatalf("expected success: %+v", resp.Error)
	}

	var data protocol.ListResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Children == nil {
		t.Error("expected non-nil children slice")
	}
	if len(data.Children) != 0 {
		t.Errorf("expected 0 children, got %d", len(data.Children))
	}
}

// TestIntegration_MultipleCommands verifies that a single connection can send
// multiple commands and receive matching responses.
func TestIntegration_MultipleCommands(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := store.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath)

	handler := server.NewDispatch(ctrl)
	srv, err := server.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	cmds := []string{
		`{"type":"ctrl_status","id":"m-1"}`,
		`{"type":"ctrl_list","id":"m-2"}`,
		`{"type":"ctrl_get","id":"m-3","childId":"nonexistent"}`,
	}
	for _, cmd := range cmds {
		if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	br := bufio.NewReader(conn)
	for _, cmd := range cmds {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read for %q: %v", cmd, err)
		}
		var resp protocol.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// m-3 is expected to fail (child not found); others succeed.
		if resp.ID == "m-3" {
			if resp.Success {
				t.Error("expected error for unknown childId")
			}
			if resp.Error == nil || resp.Error.Code != protocol.ErrChildNotFound {
				t.Errorf("expected child_not_found, got %+v", resp.Error)
			}
		} else {
			if !resp.Success {
				t.Errorf("expected success for %s, got error=%+v", resp.ID, resp.Error)
			}
		}
	}
}
