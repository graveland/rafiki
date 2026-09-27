package main

import (
	"sync"

	"go.graveland.dev/rafiki/pkg/child"
)

// ChildManager owns all live *child.Child instances. All exported methods are
// safe for concurrent use.
//
// It once also carried the framed plane's per-connection subscriber registries
// (per-child, global and label-filtered, keyed by the connection object and
// delivered to directly). The framed plane is gone — Connect's StreamEvents
// path subscribes through nativebus (Controller.nativeEventSource) — so the
// registry and its delivery fan-out retired with it.
type ChildManager struct {
	mu       sync.RWMutex
	children map[string]*child.Child
}

func newChildManager() *ChildManager {
	return &ChildManager{
		children: make(map[string]*child.Child),
	}
}

// Add registers c under childID. Called after a successful child.Spawn.
func (cm *ChildManager) Add(childID string, c *child.Child) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.children[childID] = c
}

// Get returns the live child for childID, or (nil, false) if not present.
func (cm *ChildManager) Get(childID string) (*child.Child, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	c, ok := cm.children[childID]
	return c, ok
}

// LiveIDs returns a snapshot of all currently-live child IDs.
func (cm *ChildManager) LiveIDs() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ids := make([]string, 0, len(cm.children))
	for id := range cm.children {
		ids = append(ids, id)
	}
	return ids
}

// Remove deletes the childID entry. Called on process exit.
func (cm *ChildManager) Remove(childID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.children, childID)
}
