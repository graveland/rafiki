// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestPublishEventAppendsDurableAndSkipsEphemeral(t *testing.T) {
	c := newTestController(t) // wires c.evlog = eventlog.NewMemory()
	ctx := context.Background()

	c.publishEvent("c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: "idle"}},
	})
	c.publishEvent("c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: "hi"},
		}},
	})

	recs, err := c.evlog.Read(ctx, "c_1", -1, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("len = %d, want 1 -- the delta was persisted", len(recs))
	}
	if recs[0].Type != "agent_status" {
		t.Fatalf("Type = %q, want agent_status", recs[0].Type)
	}
}

// The ordinal must reach the subscriber, not just the row: a client cursors on
// what it received, so an event delivered without its ordinal is unresumable.
func TestPublishEventStampsTheOrdinalOnTheDeliveredEvent(t *testing.T) {
	c := newTestController(t)
	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.publishEvent("c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: "idle"}},
	})

	select {
	case ev := <-ch:
		if ev.Ordinal == nil {
			t.Fatal("delivered event carries no ordinal; a subscriber cannot build a cursor from it")
		}
		if ev.GetOrdinal() != 0 {
			t.Fatalf("ordinal = %d, want 0", ev.GetOrdinal())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestSpawnPublishesNativeChildSpawned(t *testing.T) {
	c := newTestController(t)
	parent := spawnTestChild(t, c, nil)

	// Subscribe to the parent's bus and spawn a subagent under it.
	subReq := protocol.SpawnRequest{
		Kind:          protocol.KindClaude,
		Cwd:           t.TempDir(),
		PiBinary:      fakePiBin(t),
		NoSession:     true,
		ParentChildID: parent,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Spawn(ctx, subReq, users.Identity{})
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}
	childID := res.ChildID

	recs, err := c.evlog.Read(context.Background(), childID, -1, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var found *rafikiv1.ChildSpawned
	for _, r := range recs {
		if r.Type == "child_spawned" {
			var ev rafikiv1.Event
			if err := protojson.Unmarshal(r.Payload, &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			found = ev.GetChildSpawned()
		}
	}
	if found == nil {
		t.Fatal("no child_spawned event in the log")
	}
	if found.GetParentId() != parent {
		t.Errorf("parent_id = %q, want %q", found.GetParentId(), parent)
	}
	if found.GetChildId() != childID {
		t.Errorf("child_id = %q, want %q", found.GetChildId(), childID)
	}
}
