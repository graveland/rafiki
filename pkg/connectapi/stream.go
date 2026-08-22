// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// EventSource yields live ephemeral events for one child. The returned cancel
// func must be called to release the subscription.
type EventSource interface {
	Subscribe(childID string) (<-chan *rafikiv1.Event, func())
}

// SetEventSource attaches the live-event source. Without one, StreamEvents
// serves the durable replay and then ends the stream.
func (s *Server) SetEventSource(src EventSource) { s.events = src }

// StreamEvents replays the durable tier from after_ordinal, then follows live
// events. The two tiers are deliberately different: durable events carry an
// ordinal and are exactly resumable; live deltas carry none and are
// best-effort, because replaying half a token stream on reconnect is not
// something any client wants.
func (s *Server) StreamEvents(
	ctx context.Context,
	req *connect.Request[rafikiv1.StreamEventsRequest],
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	ids := req.Msg.GetChildIds()
	if len(ids) == 0 {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("at least one child_id is required"))
	}

	for _, id := range ids {
		conversationID, err := s.resolveConversation(id)
		if err != nil {
			return err
		}
		msgs, err := s.history.Load(ctx, conversationID)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if req.Msg.AfterOrdinal != nil {
			after := int(req.Msg.GetAfterOrdinal())
			kept := msgs[:0]
			for _, m := range msgs {
				if m.Ordinal > after {
					kept = append(kept, m)
				}
			}
			msgs = kept
		}
		for _, ev := range eventconv.EventsFromMessages(id, msgs) {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}

	// No live-event source is wired yet in this phase (SetEventSource has no
	// production caller — see docs/reference/control-protocol.md §2.3). Ending
	// the stream after replay, rather than blocking on ctx.Done(), avoids
	// leaking a goroutine per caller for a stream that will never deliver
	// anything; a client wanting live events reconnects/polls instead of
	// hanging with no signal that it's stuck rather than slow.
	if s.events == nil {
		return nil
	}

	ch, cancel := s.events.Subscribe(ids[0])
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
