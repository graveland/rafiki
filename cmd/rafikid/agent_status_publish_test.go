// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// handleStatusChange is the ONLY place that transitions a stored status, for
// every child kind (pi/claude via ch.DrainTransitions, fundi via the same
// StateMachine driven by its own pi-shaped frames). Before this, nothing ever
// published agent_status onto the native/rafiki-v1 stream the TUI rail reads
// -- the working spinner and glyph were frozen at whatever ListChildren
// reported at the last (re)seed, for every kind, not just fundi.
func TestHandleStatusChangePublishesAgentStatus(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	c.st.Insert(&childstore.Session{ChildID: "c_1", Status: protocol.StatusIdle, StartedAt: time.Now()})

	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusIdle)

	select {
	case ev := <-ch:
		as := ev.GetAgentStatus()
		if as == nil {
			t.Fatalf("published event has no AgentStatus payload: %+v", ev)
		}
		if as.GetState() != "streaming" {
			t.Errorf("AgentStatus.State = %q, want streaming", as.GetState())
		}
		if ev.GetChildId() != "c_1" {
			t.Errorf("ChildId = %q, want c_1", ev.GetChildId())
		}
	default:
		t.Fatal("no event published on the child's native bus")
	}
}

// A no-op status "change" (new == prev) must not spam a redundant
// agent_status onto the stream every time it is called.
func TestHandleStatusChangeSkipsAgentStatusOnNoOpTransition(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	c.st.Insert(&childstore.Session{ChildID: "c_1", Status: protocol.StatusStreaming, StartedAt: time.Now()})

	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusStreaming)

	select {
	case ev := <-ch:
		t.Fatalf("unexpected event published on a no-op status call: %+v", ev)
	default:
	}
}

// A Controller built without a native registry (several lightweight test
// fixtures in this package do exactly this) must not panic when a status
// change is handled -- publishEvent's nil-guard is what protects this.
func TestHandleStatusChangeWithNilNativeDoesNotPanic(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{ChildID: "c_1", Status: protocol.StatusIdle, StartedAt: time.Now()})
	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusIdle)
}
