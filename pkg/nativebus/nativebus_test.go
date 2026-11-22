// SPDX-License-Identifier: Apache-2.0

package nativebus_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"

	"github.com/multigres/testkit/assert"
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
		assert.NewCollecting(t).Eq("idle", ev.GetAgentStatus().GetState(), "state")
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
		assert.NewCollecting(t).Eq("idle", ev.GetAgentStatus().GetState(), "received another child's event")
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
		assert.NewCollecting(t).False(open, "received an event after cancel")
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

func TestStatsCountBusesSubscribersBacklogAndDrops(t *testing.T) {
	c := assert.NewAborting(t)
	r := nativebus.New()
	_, cancel := r.Subscribe("c_1")
	defer cancel()
	_, cancelAll := r.SubscribeAll()
	defer cancelAll()
	r.Subscribe("c_2") // never drained, never cancelled: a stuck subscriber

	st := r.Stats()
	c.Eq(2, st.Buses, "one bus per child, the daemon-wide bus excluded")
	c.Eq(3, st.Subscribers, "two per-child subscribers and one daemon-wide")
	c.Eq(uint64(0), st.Published, "nothing published yet")
	c.Eq(0, st.MaxBacklog, "empty queues")
	c.True(st.PerSubBuffer > 0, "capacity reported")

	const n = 5
	for range n {
		r.Publish("c_2", statusEvent("idle"))
	}
	st = r.Stats()
	c.Eq(uint64(n), st.Published, "published counted")
	c.Eq(n, st.MaxBacklog, "undrained subscribers hold every event")

	for range st.PerSubBuffer + 3 {
		r.Publish("c_2", statusEvent("idle"))
	}
	c.True(r.Stats().Drops > 0, "overflowing a subscriber's queue counts drops")
}

// Forgetting a bus must not subtract its drops from the cumulative total.
func TestStatsDropsSurviveForget(t *testing.T) {
	c := assert.NewAborting(t)
	r := nativebus.New()
	r.Subscribe("c_1")
	for range r.Stats().PerSubBuffer + 10 {
		r.Publish("c_1", statusEvent("idle"))
	}
	before := r.Stats().Drops
	c.True(before > 0, "setup produced drops")

	r.Forget("c_1")
	st := r.Stats()
	c.Eq(before, st.Drops, "drops outlive the bus that produced them")
	c.Eq(0, st.Buses, "the bus is gone")
}

// A cumulative counter must never step backwards between two scrapes, even when
// a bus is forgotten while Stats is reading: a bus that is in neither the live
// list nor retiredDrops for a moment, or in both, is a spurious reset to
// rate(). Several samplers hammer Stats so the window between Forget removing a
// bus and recording its drops is hit.
func TestStatsDropsNeverDecreaseWhileABusIsForgotten(t *testing.T) {
	c := assert.NewAborting(t)
	r := nativebus.New()
	const buses = 1500
	const samplers = 4
	ids := make([]string, buses)
	for i := range ids {
		ids[i] = fmt.Sprintf("c_%d", i)
		r.Subscribe(ids[i])
		for range r.Stats().PerSubBuffer + 2 {
			r.Publish(ids[i], statusEvent("idle"))
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	seen := make([][]uint64, samplers)
	for k := range samplers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					seen[k] = append(seen[k], r.Stats().Drops)
				}
			}
		}()
	}
	for _, id := range ids {
		r.Forget(id)
	}
	close(stop)
	wg.Wait()

	for k, steps := range seen {
		for i := 1; i < len(steps); i++ {
			c.True(steps[i] >= steps[i-1], "sampler %d: drops stepped backwards: %d then %d (sample %d)", k, steps[i-1], steps[i], i)
		}
	}
}
