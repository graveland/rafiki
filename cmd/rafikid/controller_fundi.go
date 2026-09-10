package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
	"go.graveland.dev/rafiki/pkg/store"
)

// conversationIDForChild resolves the conversation holding a child's persisted
// message history. A fundi child's SessionID IS the conversation UUID
// (pkg/fundi/engine.go sets SessionID: conv.ID). A claude child's SessionID is
// a session file path, so its row is found by external_ref, which is the
// child id the proxy stamped into X-Rafiki-Session
// (cmd/rafikid/controller.go proxyChildEnv). Same route pkg/insights/subtree.go
// uses to correlate claude children for cost rollup.
func (c *Controller) conversationIDForChild(snap childstore.Snapshot) string {
	if snap.Kind == protocol.KindFundi {
		return snap.SessionID
	}
	if c.pool == nil {
		return ""
	}
	var id string
	err := c.pool.QueryRow(context.Background(),
		`SELECT id::text FROM conversations.conversation
		  WHERE external_ref = $1 AND driven_by = 'client'
		  ORDER BY created_at LIMIT 1`, snap.ChildID).Scan(&id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("conversationIDForChild: lookup failed", "child", snap.ChildID, "error", err)
		}
		return ""
	}
	return id
}

// dbRecent loads a conversation's persisted messages and converts them to pi
// vocabulary. Serves BOTH fundi and claude children: DBToPiFrames emits exactly
// the message_start/message_end/tool_execution_* events that renderTranscript
// and the CLI's renderPiEvent consume, so one path serves agent_view,
// rafiki logs and rafiki tail.
//
// Not filtered by the compaction horizon, deliberately: callers render the
// full history with the boundary inline.
func (c *Controller) dbRecent(conversationID string, q control.RecentQuery) []ring.Event {
	if c.pool == nil || conversationID == "" {
		return nil
	}
	ms := store.NewMessages(c.pool)
	msgs, err := ms.Load(context.Background(), conversationID)
	if err != nil {
		slog.Warn("dbRecent: load failed", "conversationID", conversationID, "error", err)
		return nil
	}
	frames := fundi.DBToPiFrames(messageParams(msgs))

	if q.Limit > 0 && len(frames) > q.Limit {
		frames = frames[len(frames)-q.Limit:]
	}

	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}

// messageParams extracts the Param field from each store.Message.
func messageParams(msgs []store.Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, len(msgs))
	for i, m := range msgs {
		out[i] = m.Param
	}
	return out
}
