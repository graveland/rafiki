package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestIntegration_CtrlConversationStats_NoAgentDB boots the controller with a
// nil pool — matching production when FUNDI_AGENT_DB is unset — and confirms
// ctrl_conversation_stats answers no_agent_db instead of panicking on the nil
// pool. testSocketDir is defined in integration_test.go (same package).
func TestIntegration_CtrlConversationStats_NoAgentDB(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, t.Context())

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

	if _, err := conn.Write([]byte(`{"type":"ctrl_conversation_stats","id":"1"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected failure with nil pool")
	}
	if resp.Error == nil || resp.Error.Code != protocol.ErrNoAgentDB {
		t.Fatalf("expected code %s, got %+v", protocol.ErrNoAgentDB, resp.Error)
	}
}
