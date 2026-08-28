// SPDX-License-Identifier: Apache-2.0

package inbox

import "sync"

// keyedMutex serializes work per key without serializing across keys.
//
// One global mutex would be simpler and wrong: delivery writes to a child's
// command channel, which blocks when that child is wedged, so a single lock
// lets one unwell child stall delivery to every other child. Same reasoning as
// execpool's rule that nothing blocks while holding Pool.mu.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{m: make(map[string]*sync.Mutex)}
}

// lock acquires key's mutex and returns its release function.
//
// Entries are never deleted. A child id is ~30 bytes plus a mutex, the set is
// bounded by children this daemon has ever delivered to, and reference-counted
// deletion here would be a race for nothing.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}
