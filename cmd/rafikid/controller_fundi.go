package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
	"go.graveland.dev/rafiki/pkg/store"
)

// conversationIDForChild resolves the conversation holding a child's persisted
// message history. A fundi child's SessionID IS the conversation UUID
// (pkg/fundi/engine.go sets SessionID: conv.ID). A claude child's SessionID is
// its backend's own session id (the stream-json session_id the child reports,
// synced into the row by monitorChild) — not a conversation UUID and never
// resolvable as one — so its row is found by external_ref, which is the
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
// vocabulary. Serves BOTH fundi and claude children: DBToPiFramesMessages emits
// exactly the message_start/message_end/tool_execution_* events that
// renderTranscript and the CLI's renderPiEvent consume, so one path serves
// agent_view, rafiki logs and rafiki tail.
//
// Not filtered by the compaction horizon, deliberately: callers render the
// full history with the boundary inline. It IS filtered by row kind, though:
// DBToPiFramesMessages drops compaction_tail copies (the tail shows once, via
// its original row) and renders the compaction_summary row as a boundary
// divider rather than a plain user prompt.
//
// Honours q.Limit only. q.Since cannot be honoured here (DBToPiFrames stamps
// render-time timestamps, not capture time), q.Rendered has no raw alternative
// on this branch, and --raw on a resolvable child receives pi vocabulary rather
// than the child's backend frames.
func (c *Controller) dbRecent(conversationID string, q recentQuery) []ring.Event {
	if c.pool == nil || conversationID == "" {
		return nil
	}
	ms := store.NewMessages(c.pool)
	msgs, err := ms.Load(context.Background(), conversationID)
	if err != nil {
		slog.Warn("dbRecent: load failed", "conversationID", conversationID, "error", err)
		return nil
	}
	frames := fundi.DBToPiFramesMessages(msgs)

	if q.Limit > 0 && len(frames) > q.Limit {
		frames = frames[len(frames)-q.Limit:]
	}

	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}
