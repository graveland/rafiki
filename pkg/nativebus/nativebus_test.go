// SPDX-License-Identifier: Apache-2.0

package nativebus_test

import (
	"sync"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"
)

func statusEvent(state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

func TestSubscribeReceivesPublishedEvent(t *testing.T) {
	r := nativebus.New()
	ch, cancel := r.Subscribe("c_1")
	defer cancel()

	r.Publish("c_1", statusEvent("idle"))

	select {
	case ev := <-ch:
		if ev.GetAgentStatus().GetState() != "idle" {
			t.Errorf("state = %q, want idle", ev.GetAgentStatus().GetState())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the published event")
	}
}

// TestPublishIsScopedToOneChild is the isolation guarantee: a subscriber to one
// child must never see another child's events.
func TestPublishIsScopedToOneChild(t *testing.T) {
	r := nativebus.New()
	ch, cancel := r.Subscribe("c_1")
	defer cancel()

	r.Publish("c_2", statusEvent("streaming"))
	r.Publish("c_1", statusEvent("idle"))

	select {
	case ev := <-ch:
		if ev.GetAgentStatus().GetState() != "idle" {
			t.Errorf("received another child's event: %q", ev.GetAgentStatus().GetState())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestMultipleSubscribersEachReceive(t *testing.T) {
	r := nativebus.New()
	ch1, cancel1 := r.Subscribe("c_1")
	defer cancel1()
	ch2, cancel2 := r.Subscribe("c_1")
	defer cancel2()

	r.Publish("c_1", statusEvent("idle"))

	for i, ch := range []<-chan *rafikiv1.Event{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d timed out", i)
		}
	}
}

func TestPublishToUnknownChildDoesNotPanic(t *testing.T) {
	r := nativebus.New()
	r.Publish("c_nobody_listening", statusEvent("idle"))
}

func TestCancelStopsDelivery(t *testing.T) {
	r := nativebus.New()
	ch, cancel := r.Subscribe("c_1")
	cancel()

	r.Publish("c_1", statusEvent("idle"))

	select {
	case _, open := <-ch:
		if open {
			t.Error("received an event after cancel")
		}
	case <-time.After(200 * time.Millisecond):
		// Nothing delivered, which is what cancel must guarantee.
	}
}

func TestForgetReleasesTheChild(t *testing.T) {
	r := nativebus.New()
	_, cancel := r.Subscribe("c_1")
	cancel()
	r.Forget("c_1")
	// Publishing after Forget must not panic and must not resurrect the bus.
	r.Publish("c_1", statusEvent("idle"))
}

// TestConcurrentPublishAndSubscribe exists to be run under -race: the registry
// map is touched from a turn goroutine and from HTTP handlers at once.
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	r := nativebus.New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Publish("c_1", statusEvent("idle"))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, cancel := r.Subscribe("c_1")
				cancel()
			}
		}()
	}
	wg.Wait()
}
