// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// EventSource yields live events. The returned cancel func must be called to
// release the subscription.
type EventSource interface {
	Subscribe(childID string) (<-chan *rafikiv1.Event, func())
	SubscribeAll() (<-chan *rafikiv1.Event, func())
}

// eventSink is the send half of the events stream, abstracted so an
// owner-filtering wrapper can sit between the delivery loop and the wire.
type eventSink interface{ Send(*rafikiv1.Event) error }

// ownerFilteredStream drops every event whose child id the guard does not
// own, so a non-admin user's All-subject stream carries only its own
// children. An event with an empty child id is dropped too: an owned child
// always carries one, and a daemon-scoped event is not the user's to read.
// A nil event — the headers flush — passes through untouched.
type ownerFilteredStream struct {
	send  eventSink
	guard OwnerGuard
}

func (w *ownerFilteredStream) Send(ev *rafikiv1.Event) error {
	if ev == nil {
		return w.send.Send(ev)
	}
	if cid := ev.GetChildId(); cid == "" || !w.guard.Owns(cid) {
		return nil
	}
	return w.send.Send(ev)
}

func filterFromRequest(req *rafikiv1.StreamEventsRequest) (eventlog.Filter, error) {
	if req == nil || req.Subject == nil {
		return eventlog.Filter{}, errors.New("subject is required")
	}
	var subj eventlog.Subject
	switch scope := req.Subject.Scope.(type) {
	case *rafikiv1.EventSubject_Child:
		if scope.Child == "" {
			return eventlog.Filter{}, errors.New("child id is required for child subject")
		}
		subj.Scope = eventlog.ScopeChild
		subj.ChildID = scope.Child
	case *rafikiv1.EventSubject_Subtree:
		if scope.Subtree == "" {
			return eventlog.Filter{}, errors.New("subtree root id is required for subtree subject")
		}
		subj.Scope = eventlog.ScopeSubtree
		subj.ChildID = scope.Subtree
		subj.MaxDepth = int(req.Subject.MaxDepth)
		subj.IncludeSelf = req.Subject.GetIncludeSelf()
	case *rafikiv1.EventSubject_All:
		subj.Scope = eventlog.ScopeAll
	default:
		return eventlog.Filter{}, errors.New("unknown subject scope")
	}
	subj.Selector = req.Subject.LabelSelector

	var tier eventlog.Tier
	switch req.Tier {
	case rafikiv1.EventTier_EVENT_TIER_ALL:
		tier = eventlog.TierAll
	case rafikiv1.EventTier_EVENT_TIER_DURABLE, rafikiv1.EventTier_EVENT_TIER_UNSPECIFIED:
		tier = eventlog.TierDurable
	default:
		tier = eventlog.TierDurable
	}

	return eventlog.Filter{
		Subject: subj,
		Tier:    tier,
		Types:   req.Types,
	}, nil
}

// StreamEvents replays the durable tier if a cursor was supplied, then follows
// live events matching the subject predicate.
//
// childScoped: a per-child credential may stream only a filter inside its own
// subtree, resolved with the same descendant predicate every childScoped verb
// uses — a child or subtree subject rooted at the caller itself is refused
// too (a child is not a descendant of its own id), and the daemon-wide All
// subject is refused outright: it is the operator's view.
func (s *Server) StreamEvents(
	ctx context.Context,
	req *connect.Request[rafikiv1.StreamEventsRequest],
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	filter, err := filterFromRequest(req.Msg)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if c := req.Msg.GetCursor(); c != nil {
		if f := c.GetFloor(); f != nil {
			if err := f.CheckValid(); err != nil {
				return connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}
	sink := eventSink(stream)
	if sc := s.childScope(ctx); sc != nil {
		switch filter.Subject.Scope {
		case eventlog.ScopeAll:
			return refuseChildScope(errors.New("the all-subject stream is operator-only; stream your own subtree instead"))
		case eventlog.ScopeChild, eventlog.ScopeSubtree:
			if err := sc.Authorize(filter.Subject.ChildID); err != nil {
				return err
			}
		default:
			return connect.NewError(connect.CodeInvalidArgument, errors.New("unknown subject scope"))
		}
	}
	// ownerGuard: a non-admin user streams only its own children. A child or
	// subtree subject naming a foreign id is refused before any replay, with
	// the same anti-probe reasoning as the childScope block above; the All
	// subject is the user's own fleet view, filtered event by event.
	if g := s.ownerGuard(ctx); g != nil {
		switch filter.Subject.Scope {
		case eventlog.ScopeChild, eventlog.ScopeSubtree:
			if err := g.Authorize(filter.Subject.ChildID); err != nil {
				return err
			}
		case eventlog.ScopeAll:
			sink = &ownerFilteredStream{send: sink, guard: g}
		default:
			return connect.NewError(connect.CodeInvalidArgument, errors.New("unknown subject scope"))
		}
	}
	ln := s.lineage()
	if ln == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("lineage not wired"))
	}

	// The live subscription opens BEFORE replay, so an event published while
	// replay reads the log waits in the subscription instead of falling into
	// the gap between the two.
	var live <-chan *rafikiv1.Event
	if src := s.eventSource(); src != nil {
		var cancel func()
		if filter.Subject.Scope == eventlog.ScopeChild && filter.Subject.Selector == "" {
			live, cancel = src.Subscribe(filter.Subject.ChildID)
		} else {
			live, cancel = src.SubscribeAll()
		}
		defer cancel()
	}

	// Flush the response headers now. connect-go otherwise sends them with the
	// first message, so a quiet stream would send nothing at all and a client
	// could not tell a stream that opened from one that never did.
	if err := sink.Send(nil); err != nil {
		return err
	}

	d := &delivery{stream: sink, filter: filter, ln: ln, elog: s.eventLog(), seen: map[string]int32{}}

	// 1. Replay, only if a cursor was supplied.
	if c := req.Msg.GetCursor(); c != nil {
		if err := d.replay(ctx, c); err != nil {
			return err
		}
	}

	// 2. Follow live.
	if live == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-live:
			if !ok {
				return nil
			}
			if err := d.live(ctx, ev); err != nil {
				return err
			}
		}
	}
}

// replayPageSize is the per-Read page size for replay and backfill; the loop
// keeps reading until a page comes back short, so a log of any length drains.
const replayPageSize = 1000

// delivery is one stream's ordered hand-off from the durable log to the live
// bus. Replay and the live loop share seen, the highest durable ordinal this
// stream has EXAMINED per child — delivered, or read and filtered out — which
// starts at the client's cursor. Every child's log ordinals are gap-free
// (eventlog.Store.Append), so a live durable event at or below seen was
// already delivered by replay, and one beyond seen+1 means the bus dropped
// this subscriber's copies in between (pkg/bus drops rather than block a
// turn): the missing range is read back from the log first. Tracking what was
// examined rather than sent is what keeps a type-filtered stream, whose
// delivered ordinals are never contiguous, from reading the log on every
// event.
type delivery struct {
	stream eventSink
	filter eventlog.Filter
	ln     eventlog.Lineage
	elog   eventlog.Store
	seen   map[string]int32
}

// replay sends the durable events after the cursor. A child named in
// cursor.ordinals replays after its ordinal; a child subject the cursor does
// not name replays from its start. With a floor, every other child the
// subject admits replays from its first record at or after the floor — found
// through the log, because a child born during a disconnect is in no cursor —
// provided the lineage still knows it. That admits a child that spawned and
// exited inside the gap (an exited child keeps its row until it is closed)
// and never a closed one, whose log outlives it: the rails send a 24h floor on
// every reconnect, and a closed child's replayed child_spawned would otherwise
// resurrect its row. Without a floor only the named children replay:
// unbounded would mean every child the caller can see, from its first event.
func (d *delivery) replay(ctx context.Context, cursor *rafikiv1.EventCursor) error {
	if d.elog == nil {
		return nil
	}
	floor := timestampTime(cursor.GetFloor())

	after := make(map[string]int32)
	for cid, ord := range cursor.GetOrdinals() {
		after[cid] = ord
	}
	if d.filter.Subject.Scope == eventlog.ScopeChild {
		if _, ok := after[d.filter.Subject.ChildID]; !ok {
			after[d.filter.Subject.ChildID] = -1
		}
	} else if !floor.IsZero() {
		since, err := d.elog.ChildrenSince(ctx, floor)
		if err != nil {
			return unmappedErr(err, "rpc", "StreamEvents", "phase", "replay children since floor")
		}
		for cid, first := range since {
			if _, named := after[cid]; named {
				continue
			}
			if _, known := d.ln.Labels(cid); known && d.filter.AdmitsChild(cid, d.ln) {
				after[cid] = first - 1
			}
		}
	}

	for cid, ord := range after {
		d.seen[cid] = ord
		if err := d.read(ctx, cid, ord, noBound, floor, "replay"); err != nil {
			return err
		}
	}
	return nil
}

// live delivers one bus event. An event without an ordinal (the ephemeral
// tier, or a durable event whose log append failed) has no place in the
// sequence and passes straight to the filter.
func (d *delivery) live(ctx context.Context, ev *rafikiv1.Event) error {
	if ev.Ordinal == nil {
		return d.send(ev)
	}
	cid, ord := ev.GetChildId(), ev.GetOrdinal()
	last, known := d.seen[cid]
	if known && ord <= last {
		return nil
	}
	if known && ord > last+1 && d.elog != nil {
		if err := d.read(ctx, cid, last, ord-1, time.Time{}, "backfill"); err != nil {
			return err
		}
	}
	d.seen[cid] = ord
	return d.send(ev)
}

// noBound is read's "to the end of the log".
const noBound int32 = -1

// read sends cid's logged events after ordinal `after` through `through`
// (noBound for the end of the log), skipping any created before floor. Every
// record read advances seen, sent or not.
func (d *delivery) read(ctx context.Context, cid string, after, through int32, floor time.Time, phase string) error {
	for {
		recs, err := d.elog.Read(ctx, cid, after, replayPageSize)
		if err != nil {
			return unmappedErr(err, "rpc", "StreamEvents", "phase", phase+" read", "childId", cid)
		}
		for _, rec := range recs {
			if through != noBound && rec.Ordinal > through {
				return nil
			}
			after = rec.Ordinal
			d.seen[cid] = rec.Ordinal
			if !floor.IsZero() && rec.CreatedAt.Before(floor) {
				continue
			}
			ev, err := rec.Decode()
			if err != nil {
				return unmappedErr(err, "rpc", "StreamEvents", "phase", phase+" decode", "childId", cid, "ordinal", rec.Ordinal)
			}
			ev.Ordinal = &rec.Ordinal
			if ev.ChildId == "" {
				ev.ChildId = cid
			}
			if err := d.send(ev); err != nil {
				return err
			}
		}
		// A short page means the log is drained. A full page loops again;
		// the next Read simply starts past the last ordinal seen, so an
		// empty log tail can never spin the loop.
		if len(recs) < replayPageSize {
			return nil
		}
	}
}

func (d *delivery) send(ev *rafikiv1.Event) error {
	if !d.filter.Match(ev, d.ln) {
		return nil
	}
	return d.stream.Send(ev)
}
