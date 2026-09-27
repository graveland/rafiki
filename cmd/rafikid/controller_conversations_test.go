package main

import (
	"errors"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestConversationStatsNoAgentDB boots the controller with a nil pool —
// matching production when RAFIKI_DB is unset — and confirms
// ConversationStats answers no_agent_db instead of panicking on the nil pool.
func TestConversationStatsNoAgentDB(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	dir := testSocketDir(t)
	st := childstore.New()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, nil, nil, false, t.Context(), nil, nil, nil, nil)

	_, err := ctrl.ConversationStats(t.Context(), insights.ScopeAll(), insights.StatsFilter{})
	var ce *connectapi.ControllerError
	c.True(errors.As(err, &ce), "expected *connectapi.ControllerError, got %T: %v", err, err)
	c.Eq(protocol.ErrNoAgentDB, ce.Code, "expected code")
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
		filepath.Join(dir, "c.sock"), nil, nil, nil, false, t.Context(), nil, nil, nil, nil)

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
		assert.NewCollecting(t).False(ok != tc.wantOK || got != tc.want, "ConversationID(%q) = (%q, %v), want (%q, %v)", tc.childID, got, ok, tc.want, tc.wantOK)
	}
}
