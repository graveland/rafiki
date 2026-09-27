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

	"github.com/multigres/testkit/assert"
)

func TestPublishEventAppendsDurableAndSkipsEphemeral(t *testing.T) {
	ck := assert.NewAborting(t)
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
	ck.NoError(err, "Read")
	ck.Len(recs, 1, "len = %d, want 1 -- the delta was persisted", len(recs))
	ck.Eq("agent_status", recs[0].Type, "Type")
}

// The ordinal must reach the subscriber, not just the row: a client cursors on
// what it received, so an event delivered without its ordinal is unresumable.
func TestPublishEventStampsTheOrdinalOnTheDeliveredEvent(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.publishEvent("c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: "idle"}},
	})

	select {
	case ev := <-ch:
		ck.NotNil(ev.Ordinal, "delivered event carries no ordinal; a subscriber cannot build a cursor from it")
		ck.Eq(0, ev.GetOrdinal(), "ordinal")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestSpawnPublishesNativeChildSpawned(t *testing.T) {
	ck := assert.NewCollecting(t)
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
	ck.Require().NoError(err, "spawn subagent")
	childID := res.ChildID

	recs, err := c.evlog.Read(context.Background(), childID, -1, 0)
	ck.Require().NoError(err, "Read")
	var found *rafikiv1.ChildSpawned
	for _, r := range recs {
		if r.Type == "child_spawned" {
			var ev rafikiv1.Event
			ck.Require().NoError(protojson.Unmarshal(r.Payload, &ev), "unmarshal")
			found = ev.GetChildSpawned()
		}
	}
	ck.Require().NotNil(found, "no child_spawned event in the log")
	ck.Eq(parent, found.GetParentId(), "parent_id")
	ck.Eq(childID, found.GetChildId(), "child_id")
}
