// SPDX-License-Identifier: Apache-2.0

// Package eventlogtest provides the conformance suite that every
// eventlog.Store implementation must pass unchanged.
//
// The caveat that applies to pkg/tasks applies here verbatim and is worth
// re-reading before trusting a green run: the memory store is atomic under its
// own mutex and Postgres is not, so this suite structurally CANNOT catch a
// Postgres-only race. Concurrent-append defects belong in
// pkg/eventlogdb/postgres_test.go, which opens two transactions explicitly.
package eventlogtest

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func statusEvent(childID, state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

func RunConformance(t *testing.T, mk func(*testing.T) (eventlog.Store, string)) {
	ctx := context.Background()

	t.Run("append assigns sequential ordinals from zero", func(t *testing.T) {
		s, child := mk(t)
		for want := range int32(3) {
			got, err := s.Append(ctx, child, statusEvent(child, "idle"))
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if got != want {
				t.Fatalf("ordinal = %d, want %d", got, want)
			}
		}
	})

	t.Run("ordinals are per child, not global", func(t *testing.T) {
		s, child := mk(t)
		if _, err := s.Append(ctx, child, statusEvent(child, "idle")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		// A second child starts its own sequence at zero.
		other := child + "_other"
		got, err := s.Append(ctx, other, statusEvent(other, "idle"))
		if err != nil {
			t.Fatalf("Append other: %v", err)
		}
		if got != 0 {
			t.Fatalf("second child's first ordinal = %d, want 0 — ordinals leaked across children", got)
		}
	})

	t.Run("read filters by after_ordinal", func(t *testing.T) {
		s, child := mk(t)
		for range 5 {
			if _, err := s.Append(ctx, child, statusEvent(child, "idle")); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		recs, err := s.Read(ctx, child, 2, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(recs) != 2 {
			t.Fatalf("len = %d, want 2 (ordinals 3 and 4)", len(recs))
		}
		if recs[0].Ordinal != 3 || recs[1].Ordinal != 4 {
			t.Fatalf("ordinals = %d,%d; want 3,4", recs[0].Ordinal, recs[1].Ordinal)
		}
	})

	t.Run("read honours limit and stays ascending", func(t *testing.T) {
		s, child := mk(t)
		for range 5 {
			if _, err := s.Append(ctx, child, statusEvent(child, "idle")); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		recs, err := s.Read(ctx, child, -1, 2)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(recs) != 2 || recs[0].Ordinal != 0 || recs[1].Ordinal != 1 {
			t.Fatalf("got %+v, want the first two in ascending order", recs)
		}
	})

	t.Run("latest reports the highest ordinal", func(t *testing.T) {
		s, child := mk(t)
		if _, err := s.Latest(ctx, child); !errors.Is(err, eventlog.ErrNotFound) {
			t.Fatalf("Latest on an empty child = %v, want ErrNotFound", err)
		}
		for range 3 {
			if _, err := s.Append(ctx, child, statusEvent(child, "idle")); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		got, err := s.Latest(ctx, child)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if got != 2 {
			t.Fatalf("Latest = %d, want 2", got)
		}
	})

	t.Run("appending an ephemeral event is refused", func(t *testing.T) {
		s, child := mk(t)
		delta := &rafikiv1.Event{
			ChildId: child,
			Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{}},
		}
		if _, err := s.Append(ctx, child, delta); err == nil {
			t.Fatal("Append accepted an ephemeral event; the tier split is not enforced at the store")
		}
	})

	t.Run("payload round-trips", func(t *testing.T) {
		s, child := mk(t)
		if _, err := s.Append(ctx, child, statusEvent(child, "tool_running")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		recs, err := s.Read(ctx, child, -1, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("len = %d, want 1", len(recs))
		}
		if recs[0].Type != "agent_status" {
			t.Fatalf("Type = %q, want agent_status", recs[0].Type)
		}
		var ev rafikiv1.Event
		if err := protojson.Unmarshal(recs[0].Payload, &ev); err != nil {
			t.Fatalf("payload does not unmarshal: %v", err)
		}
		if ev.GetAgentStatus().GetState() != "tool_running" {
			t.Fatalf("state = %q, want tool_running", ev.GetAgentStatus().GetState())
		}
	})
}
