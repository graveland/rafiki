// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// EventSource yields live events. The returned cancel func must be called to
// release the subscription.
type EventSource interface {
	Subscribe(childID string) (<-chan *rafikiv1.Event, func())
	SubscribeAll() (<-chan *rafikiv1.Event, func())
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
func (s *Server) StreamEvents(
	ctx context.Context,
	req *connect.Request[rafikiv1.StreamEventsRequest],
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	filter, err := filterFromRequest(req.Msg)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	ln := s.lineage()
	if ln == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("lineage not wired"))
	}

	// 1. Replay, only if a cursor was supplied.
	if c := req.Msg.GetCursor(); c != nil {
		if err := s.replay(ctx, filter, c, ln, stream); err != nil {
			return err
		}
	}

	// 2. Follow live.
	src := s.eventSource()
	if src == nil {
		return nil
	}
	var ch <-chan *rafikiv1.Event
	var cancel func()
	if filter.Subject.Scope == eventlog.ScopeChild && filter.Subject.Selector == "" {
		ch, cancel = src.Subscribe(filter.Subject.ChildID)
	} else {
		ch, cancel = src.SubscribeAll()
	}
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if !filter.Match(ev, ln) {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (s *Server) replay(
	ctx context.Context,
	filter eventlog.Filter,
	cursor *rafikiv1.EventCursor,
	ln eventlog.Lineage,
	stream *connect.ServerStream[rafikiv1.Event],
) error {
	elog := s.eventLog()
	if elog == nil {
		return nil
	}

	var floorTime time.Time
	if cursor.FloorUnixMs > 0 {
		floorTime = time.UnixMilli(cursor.FloorUnixMs)
	}

	// Determine children to replay.
	// For ScopeChild: only that child.
	// For other scopes or cursor ordinals map: iterate keys in cursor.Ordinals.
	childrenToReplay := make(map[string]int32)
	if cursor.Ordinals != nil {
		for cid, ord := range cursor.Ordinals {
			childrenToReplay[cid] = ord
		}
	}
	if filter.Subject.Scope == eventlog.ScopeChild {
		if _, ok := childrenToReplay[filter.Subject.ChildID]; !ok {
			childrenToReplay[filter.Subject.ChildID] = -1
		}
	}

	for cid, lastOrd := range childrenToReplay {
		afterOrd := lastOrd
		recs, err := elog.Read(ctx, cid, afterOrd, 1000)
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("replay read child %s: %w", cid, err))
		}
		for _, rec := range recs {
			if !floorTime.IsZero() && rec.CreatedAt.Before(floorTime) {
				continue
			}
			var ev rafikiv1.Event
			if err := protojson.Unmarshal(rec.Payload, &ev); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("unmarshal event %s:%d: %w", cid, rec.Ordinal, err))
			}
			ev.Ordinal = &rec.Ordinal
			if !filter.Match(&ev, ln) {
				continue
			}
			if err := stream.Send(&ev); err != nil {
				return err
			}
		}
	}
	return nil
}
