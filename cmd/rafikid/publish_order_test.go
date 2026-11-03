// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"

	"github.com/multigres/testkit/assert"
)

// delayedAppendStore allocates the next ordinal the way eventlogdb does (under
// a lock, so no two appends share one) and then DELAYS the first append's
// return. That is the window publishEvent must not straddle: the allocation is
// already ordered, the publish that follows it is not.
type delayedAppendStore struct {
	mu    sync.Mutex
	next  map[string]int32
	delay time.Duration
}

func newDelayedAppendStore(delay time.Duration) *delayedAppendStore {
	return &delayedAppendStore{next: map[string]int32{}, delay: delay}
}

func (s *delayedAppendStore) Append(_ context.Context, childID string, _ *rafikiv1.Event) (int32, error) {
	s.mu.Lock()
	ord := s.next[childID]
	s.next[childID] = ord + 1
	s.mu.Unlock()
	if ord == 0 {
		time.Sleep(s.delay)
	}
	return ord, nil
}

func (s *delayedAppendStore) Read(context.Context, string, int32, int) ([]eventlog.Record, error) {
	return nil, nil
}

func (s *delayedAppendStore) Latest(context.Context, string) (int32, error) {
	return 0, eventlog.ErrNotFound
}

func (s *delayedAppendStore) ChildrenSince(context.Context, time.Time) (map[string]int32, error) {
	return nil, nil
}

// A child's durable events must reach subscribers in ORDINAL order, not merely
// with distinct ordinals.
//
// Ordinal allocation is serialized (eventlogdb takes a per-child advisory lock)
// but the publish that follows it is a separate step, so a publisher descheduled
// between the two lets a later append publish first. Every cursor-based consumer
// then sees the earlier event at or below its cursor and drops it as already
// delivered — connectapi's delivery.live does it, and so does the cockpit's
// session. A dropped user_message is a message that never appears in the
// transcript AND a pending ghost that never clears, which is the bug this test
// exists to keep shut.
func TestPublishEventDeliversDurableEventsInOrdinalOrder(t *testing.T) {
	ck := assert.NewAborting(t)
	reg := nativebus.New()
	c := &Controller{native: reg, evlog: newDelayedAppendStore(50 * time.Millisecond)}

	ch, cancel := reg.Subscribe("c_1")
	defer cancel()

	const n = 2
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.publishEvent("c_1", &rafikiv1.Event{
				ChildId: "c_1",
				Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{
					State: "streaming",
				}},
			})
		}()
	}
	wg.Wait()

	var got []int32
	for range n {
		select {
		case ev := <-ch:
			ck.Require().NotNil(ev.Ordinal, "published event carries no ordinal")
			got = append(got, ev.GetOrdinal())
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d events published", len(got), n)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("events published out of ordinal order: %v; a cursor-based consumer drops the late one as a duplicate", got)
		}
	}
}

// A durable event whose log append FAILED must not be published carrying a
// stale ordinal. Capture-built events (eventconv.EventsFromNodes) arrive with
// their CONVERSATION ordinal already set; left in place it is a small number
// every consumer has already passed, so the event is dropped as a duplicate and
// the message it carries disappears with no error anywhere. An ordinal-less
// event is delivered and applied by everyone.
func TestPublishEventClearsAStaleOrdinalWhenTheAppendFails(t *testing.T) {
	ck := assert.NewAborting(t)
	reg := nativebus.New()
	c := &Controller{native: reg, evlog: failingAppendStore{}}

	ch, cancel := reg.Subscribe("c_1")
	defer cancel()

	stale := int32(7)
	c.publishEvent("c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Ordinal: &stale,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{}},
	})

	select {
	case ev := <-ch:
		ck.Nil(ev.Ordinal, "a failed append left the conversation ordinal on the published event")
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}
}

type failingAppendStore struct{}

func (failingAppendStore) Append(context.Context, string, *rafikiv1.Event) (int32, error) {
	return 0, context.DeadlineExceeded
}

func (failingAppendStore) Read(context.Context, string, int32, int) ([]eventlog.Record, error) {
	return nil, nil
}

func (failingAppendStore) Latest(context.Context, string) (int32, error) {
	return 0, eventlog.ErrNotFound
}

func (failingAppendStore) ChildrenSince(context.Context, time.Time) (map[string]int32, error) {
	return nil, nil
}
