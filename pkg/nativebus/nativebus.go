// SPDX-License-Identifier: Apache-2.0

// Package nativebus fans rafiki-native events out per child.
//
// It lives here rather than on child.Child because the producers are all
// daemon-side: fundi's Emitter runs in-process on a turn goroutine, and
// claude's stream-json is parsed in the daemon too. Keeping the registry out
// of pkg/child also keeps the generated protobuf package out of pkg/child's
// import graph.
package nativebus

import (
	"sync"

	"go.graveland.dev/rafiki/pkg/bus"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Registry holds one bus per child, plus a daemon-wide allBus.
type Registry struct {
	mu     sync.Mutex
	buses  map[string]*bus.Bus[*rafikiv1.Event]
	allBus *bus.Bus[*rafikiv1.Event]
}

// New builds an empty Registry.
func New() *Registry {
	return &Registry{
		buses:  make(map[string]*bus.Bus[*rafikiv1.Event]),
		allBus: bus.New[*rafikiv1.Event](bus.Options{}),
	}
}

// busFor returns the child's bus, creating it if needed.
func (r *Registry) busFor(childID string) *bus.Bus[*rafikiv1.Event] {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buses[childID]
	if !ok {
		b = bus.New[*rafikiv1.Event](bus.Options{})
		r.buses[childID] = b
	}
	return b
}

// Publish delivers ev to every subscriber of childID and to the daemon-wide fan-out.
// A slow subscriber has its copy dropped rather than blocking the turn — that is
// pkg/bus's documented behaviour and the reason this wraps it instead of reimplementing.
//
// The registry lock is released before Publish runs: bus.Publish walks a
// dispatch loop, and holding the map lock across it would serialize every
// child's emission behind one slow subscriber.
func (r *Registry) Publish(childID string, ev *rafikiv1.Event) {
	r.busFor(childID).Publish(ev)
	if r.allBus != nil {
		r.allBus.Publish(ev)
	}
}

// Subscribe returns a channel of the child's events and a cancel func that
// must be called to release the subscription. The signature deliberately
// matches connectapi.EventSource.
func (r *Registry) Subscribe(childID string) (<-chan *rafikiv1.Event, func()) {
	return r.busFor(childID).Subscribe()
}

// SubscribeAll returns a channel receiving all daemon-wide events and a cancel func.
func (r *Registry) SubscribeAll() (<-chan *rafikiv1.Event, func()) {
	return r.allBus.Subscribe()
}

// Forget drops a child's bus. Call it when a child is removed so a long-lived
// daemon does not accumulate one bus per child it has ever run.
func (r *Registry) Forget(childID string) {
	r.mu.Lock()
	b, ok := r.buses[childID]
	delete(r.buses, childID)
	r.mu.Unlock()
	if ok {
		b.Close()
	}
}
