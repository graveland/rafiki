package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/version"
)

// testSocketDir returns a temp directory with a short path (macOS UDS paths
// are capped at 104 bytes; t.TempDir() names exceed this for long test names).
func testSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rafiki")
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

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, t.Context(), nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

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
	if data.Version != version.String() {
		t.Errorf("version: want %s, got %s", version.String(), data.Version)
	}
}

// TestIntegration_CtrlList boots the controller, sends ctrl_list, verifies
// the response contains an empty children array (no children have been spawned).
func TestIntegration_CtrlList(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

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

// TestIntegration_SetLabels_Success inserts a session, calls ctrl_set_labels,
// and verifies the labels are updated and returned.
func TestIntegration_SetLabels_Success(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	// Insert a session manually.
	now := time.Now()
	sess := &childstore.Session{
		ChildID:   "c_label_integ",
		Status:    "idle",
		Cwd:       "/tmp",
		StartedAt: now,
	}
	st.Insert(sess)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	frame := `{"type":"ctrl_set_labels","id":"lbl-1","childId":"c_label_integ","set":{"env":"prod","tier":"fast"}}` + "\n"
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

	var data protocol.SetLabelsResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Labels["env"] != "prod" || data.Labels["tier"] != "fast" {
		t.Errorf("unexpected labels: %v", data.Labels)
	}

	// Verify via ctrl_get that the labels are persisted in the store.
	snap, ok := st.Get("c_label_integ")
	if !ok {
		t.Fatal("session not found after set_labels")
	}
	if snap.Labels["env"] != "prod" {
		t.Errorf("store label env: got %q", snap.Labels["env"])
	}
}

// TestIntegration_SetLabels_ReservedPrefix verifies that rafiki/ keys are rejected.
func TestIntegration_SetLabels_ReservedPrefix(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	now := time.Now()
	st.Insert(&childstore.Session{ChildID: "c_reserved", Status: "idle", Cwd: "/tmp", StartedAt: now})

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	frame := `{"type":"ctrl_set_labels","id":"lbl-2","childId":"c_reserved","set":{"rafiki/model":"evil"}}` + "\n"
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
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected error for rafiki/ prefix, got success")
	}
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidArgs {
		t.Errorf("expected invalid_args, got %+v", resp.Error)
	}
}

// TestIntegration_SetLabels_InvalidKey verifies that malformed keys are rejected.
func TestIntegration_SetLabels_InvalidKey(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	now := time.Now()
	st.Insert(&childstore.Session{ChildID: "c_badkey", Status: "idle", Cwd: "/tmp", StartedAt: now})

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Key with space is invalid.
	frame := `{"type":"ctrl_set_labels","id":"lbl-3","childId":"c_badkey","set":{"bad key":"v"}}` + "\n"
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
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected error for bad key, got success")
	}
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidArgs {
		t.Errorf("expected invalid_args, got %+v", resp.Error)
	}
}

// TestIntegration_List_LabelFilter verifies that ctrl_list honours label filters.
func TestIntegration_List_LabelFilter(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	now := time.Now()
	sessA := &childstore.Session{ChildID: "c_a", Status: "idle", Cwd: "/a", StartedAt: now,
		Labels: map[string]string{"env": "prod", "tier": "fast"}}
	sessB := &childstore.Session{ChildID: "c_b", Status: "idle", Cwd: "/b", StartedAt: now,
		Labels: map[string]string{"env": "staging"}}
	st.Insert(sessA)
	st.Insert(sessB)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Filter by env=prod: should return only c_a.
	frame := `{"type":"ctrl_list","id":"lbl-list-1","filter":{"labels":{"env":"prod"}}}` + "\n"
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
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success: %+v", resp.Error)
	}
	var data protocol.ListResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if len(data.Children) != 1 || data.Children[0].ChildID != "c_a" {
		t.Errorf("label filter: got %d children, expected c_a only; got %v", len(data.Children), data.Children)
	}
}

// TestIntegration_MultipleCommands verifies that a single connection can send
// multiple commands and receive matching responses.
func TestIntegration_MultipleCommands(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

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

// TestIntegration_Subscribe_LabelFiltered verifies that a label-filtered
// ctrl_subscribe succeeds and that combining childId + labels returns the
// mutually-exclusive error.
func TestIntegration_Subscribe_LabelFiltered(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), socketPath, nil, nil, nil, t.Context(), nil)

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	br := bufio.NewReader(conn)

	// Label-filtered subscribe: should succeed.
	if _, err := conn.Write([]byte(`{"type":"ctrl_subscribe","id":"lf-1","labels":{"context":"work"}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp protocol.Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success {
		t.Fatalf("label-filtered subscribe: expected success, got %+v", resp.Error)
	}

	// hasLabel-only subscribe: should succeed.
	if _, err := conn.Write([]byte(`{"type":"ctrl_subscribe","id":"lf-2","hasLabel":["tier"]}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success {
		t.Fatalf("hasLabel subscribe: expected success, got %+v", resp.Error)
	}

	// childId + labels: mutually exclusive error.
	if _, err := conn.Write([]byte(`{"type":"ctrl_subscribe","id":"lf-3","childId":"c_x","labels":{"env":"prod"}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected error for childId+labels, got success")
	}
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidArgs {
		t.Errorf("expected invalid_args, got %+v", resp.Error)
	}
	if resp.Error != nil && resp.Error.Message != "subscribe: childId and labels are mutually exclusive" {
		t.Errorf("unexpected error message: %s", resp.Error.Message)
	}
}
