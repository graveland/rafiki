// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// eventStream holds the streaming connection and the daemon client.
type eventStream struct {
	client  rafikiv1connect.ControlClient
	ch      chan *rafikiv1.Event
	childID string
	cancel  context.CancelFunc
}

// startStream opens GetHistory then StreamEvents in a background goroutine.
// Events arrive on ch. The returned cancel func stops the stream.
func startStream(httpClient *http.Client, baseURL string, childID string) (*eventStream, error) {
	client := rafikiv1connect.NewControlClient(httpClient, baseURL)
	ch := make(chan *rafikiv1.Event, 128)
	ctx, cancel := context.WithCancel(context.Background())

	es := &eventStream{
		client:  client,
		ch:      ch,
		childID: childID,
		cancel:  cancel,
	}

	go es.run(ctx)
	return es, nil
}

func (es *eventStream) run(ctx context.Context) {
	defer close(es.ch)

	// Replay durable history first.
	historyResp, err := es.client.GetHistory(ctx,
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: es.childID}))
	if err != nil {
		slog.Warn("tui: get history failed", "child", es.childID, "error", err)
		es.ch <- &rafikiv1.Event{
			ChildId: es.childID,
			Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
				Code: "history_failed", Message: err.Error(),
			}},
		}
		return
	}

	var lastOrdinal int32
	for _, ev := range historyResp.Msg.GetEvents() {
		select {
		case <-ctx.Done():
			return
		case es.ch <- ev:
		}
		if ev.Ordinal != nil {
			lastOrdinal = ev.GetOrdinal()
		}
	}

	// Now stream live events.
	stream, err := es.client.StreamEvents(ctx,
		connect.NewRequest(&rafikiv1.StreamEventsRequest{
			Subject: &rafikiv1.EventSubject{
				Scope: &rafikiv1.EventSubject_Child{Child: es.childID},
			},
			Tier:   rafikiv1.EventTier_EVENT_TIER_ALL,
			Cursor: &rafikiv1.EventCursor{Ordinals: map[string]int32{es.childID: lastOrdinal}},
		}))
	if err != nil {
		slog.Warn("tui: stream events failed", "child", es.childID, "error", err)
		return
	}

	for stream.Receive() {
		select {
		case <-ctx.Done():
			return
		case es.ch <- stream.Msg():
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("tui: stream ended with error", "child", es.childID, "error", err)
	}
}
