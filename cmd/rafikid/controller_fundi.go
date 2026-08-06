package main

import (
	"context"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/ring"
	"go.graveland.dev/rafiki/pkg/store"
)

// dbRecentForFundi loads conversation messages from the database and converts
// them to pi-vocabulary frames for ctrl_get_recent. This is the canonical read
// path for fundi children — the ring buffer is bypassed entirely. Returns nil
// when the pool is nil (no DB configured).
func (c *Controller) dbRecentForFundi(conversationID string, q control.RecentQuery) []ring.Event {
	if c.pool == nil || conversationID == "" {
		return nil
	}
	ms := store.NewMessages(c.pool)
	msgs, err := ms.Load(context.Background(), conversationID)
	if err != nil {
		slog.Warn("dbRecentForFundi: load failed", "conversationID", conversationID, "error", err)
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
