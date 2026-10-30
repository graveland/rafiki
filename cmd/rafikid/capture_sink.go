package main

import (
	"log/slog"
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
)

// capturePublisher turns capture reports into durable message events. mark
// is the highest ordinal published per conversation: the read-back that
// built a report can include a row an overlapping report already
// published (request N+1's read-back sees N's reply), and each ordinal
// publishes once, in ordinal order.
type capturePublisher struct {
	mu   sync.Mutex
	mark map[string]int
}

// heldEventReleaser is the slice of *child.Child a capture report drives: it
// names the response now persisted, or frees whatever the gate still holds.
type heldEventReleaser interface {
	MessagePersisted(messageID string)
	ReleaseHeldEvents()
}

// gateFor resolves a live child's gate. The explicit false return keeps a
// missing child from surfacing as a non-nil interface wrapping a nil
// *child.Child.
func (c *Controller) gateFor(childID string) (heldEventReleaser, bool) {
	if c.gateLookup != nil {
		return c.gateLookup(childID)
	}
	ch, ok := c.cm.Get(childID)
	if !ok || ch == nil {
		return nil, false
	}
	return ch, true
}

// CaptureReported implements server.CaptureSink. It publishes the report's
// stored rows as message events and then tells the child's gate which
// response is now persisted. The gate is driven only after capturePub.mu is
// released: the gate invokes publishEvent under its own lock, so holding
// capturePub.mu across a gate call would order the two locks both ways.
//
// A ReleaseOnly report publishes nothing at all — its request is not the
// agent's own conversation (a titler, a quota probe, a per-tool helper) — but
// it still drives the gate: the response it names is what a frame on the
// child's stdout armed the gate behind, and dropping that report is what left
// the child's event stream silent until it exited.
func (c *Controller) CaptureReported(r server.CaptureReport) {
	childID := r.Session
	if r.ThreadID != "" {
		childID = threadChildID(r.Session, r.ThreadID)
	}
	if _, ok := c.st.Get(childID); !ok {
		slog.Debug("capture report for unknown child", "child", childID, "conversation", r.ConversationID)
		return
	}

	if r.ReleaseOnly {
		// No rows and no capture_not_persisted Error: this request's messages
		// belong to another conversation, and a helper's capture failure is not
		// the agent's turn. Only the gate release below applies to it.
		r.Rows = nil
	}

	c.capturePub.mu.Lock()
	if c.capturePub.mark == nil {
		c.capturePub.mark = make(map[string]int)
	}
	kept := r.Rows
	if mark, ok := c.capturePub.mark[r.ConversationID]; ok {
		kept = nil
		for _, m := range r.Rows {
			if m.Ordinal > mark {
				kept = append(kept, m)
			}
		}
	}
	// The translator already published this compaction's boundary live.
	live := make([]*conv.Node, 0, len(kept))
	for _, m := range kept {
		if m.Kind != conv.KindCompactionSummary {
			live = append(live, m)
		}
	}
	for _, ev := range eventconv.EventsFromNodes(childID, live) {
		c.publishEvent(childID, ev)
	}
	if len(kept) > 0 {
		c.capturePub.mark[r.ConversationID] = kept[len(kept)-1].Ordinal
	}
	if !r.ReleaseOnly {
		for _, e := range []struct{ code, prefix, text string }{
			{"turn_failed", "turn failed: ", r.TurnErr},
			{"capture_not_persisted", "turn not persisted: ", r.Err},
		} {
			if e.text == "" {
				continue
			}
			c.publishEvent(childID, &rafikiv1.Event{
				ChildId: childID,
				Ts:      timestamppb.Now(),
				Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
					Code:    e.code,
					Message: e.prefix + e.text,
				}},
			})
		}
	}
	c.capturePub.mu.Unlock()

	if r.ThreadID != "" {
		return
	}
	g, ok := c.gateFor(childID)
	if !ok {
		return
	}
	if r.Err == "" && r.TurnErr == "" && r.ResponseMessageID != "" {
		g.MessagePersisted(r.ResponseMessageID)
		return
	}
	if r.ReleaseOnly {
		// A release-only report only ever releases the one response it names:
		// with no id there is nothing of THIS request to release, and flushing
		// here would publish the agent's own held turn events out of order
		// behind a helper's failure. The gate's stall release is the backstop
		// for a request that armed it and could not name its response.
		slog.Debug("capture report: release-only request stored no response id; gate left to its backstop",
			"child", childID, "conversation", r.ConversationID, "error", r.Err)
		return
	}
	g.ReleaseHeldEvents()
}
