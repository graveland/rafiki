// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// EventSource yields live ephemeral events for one child. The returned cancel
// func must be called to release the subscription.
type EventSource interface {
	Subscribe(childID string) (<-chan *rafikiv1.Event, func())
}

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

	src := s.eventSource()
	// No live-event source wired: end the stream after replay rather than
	// blocking on ctx.Done(), which would leak a goroutine per caller for a
	// stream that can never deliver anything.
	if src == nil {
		return nil
	}

	// Subscribe to EVERY requested child, not just the first. Following only
	// ids[0] silently drops the other children's events, which looks to the
	// caller like those agents went quiet.
	merged := make(chan *rafikiv1.Event)
	var wg sync.WaitGroup
	streamCtx, stopFanIn := context.WithCancel(ctx)
	defer stopFanIn()

	for _, id := range ids {
		ch, cancel := src.Subscribe(id)
		defer cancel()
		wg.Add(1)
		go func(ch <-chan *rafikiv1.Event) {
			defer wg.Done()
			for {
				select {
				case <-streamCtx.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						return
					}
					select {
					case merged <- ev:
					case <-streamCtx.Done():
						return
					}
				}
			}
		}(ch)
	}

	// Close merged once every fan-in goroutine has stopped, so the send loop
	// below terminates instead of blocking forever on a dead stream.
	go func() {
		wg.Wait()
		close(merged)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-merged:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
