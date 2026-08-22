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
// nil pool — matching production when RAFIKI_DB is unset — and confirms
// ctrl_conversation_stats answers no_agent_db instead of panicking on the nil
// pool. testSocketDir is defined in integration_test.go (same package).
func TestIntegration_CtrlConversationStats_NoAgentDB(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, nil, t.Context(), nil, nil, nil)

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

// Controller.ConversationID backs connectapi.ConversationResolver, whose whole
// job is keeping a child id out of a query that reads
// WHERE conversation_id = $1::uuid. Only fundi children carry a conversation
// UUID in SessionID; a pi or claude child's SessionID is a session file id,
// so resolving one would reintroduce the bug in another costume.
func TestControllerConversationIDOnlyResolvesFundiChildren(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, nil, nil, t.Context(), nil, nil, nil)

	const conversationUUID = "1e3f4a9c-0000-4000-8000-000000000001"
	st.Insert(&childstore.Session{
		ChildID: "c_fundi", Kind: protocol.KindFundi, SessionID: conversationUUID,
	})
	st.Insert(&childstore.Session{
		ChildID: "c_claude", Kind: protocol.KindClaude, SessionID: "some-claude-session",
	})
	st.Insert(&childstore.Session{
		ChildID: "c_fundi_nosession", Kind: protocol.KindFundi,
	})

	for _, tc := range []struct {
		childID string
		want    string
		wantOK  bool
	}{
		{"c_fundi", conversationUUID, true},
		{"c_claude", "", false},
		{"c_fundi_nosession", "", false},
		{"c_missing", "", false},
	} {
		got, ok := ctrl.ConversationID(tc.childID)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("ConversationID(%q) = (%q, %v), want (%q, %v)",
				tc.childID, got, ok, tc.want, tc.wantOK)
		}
	}
}
