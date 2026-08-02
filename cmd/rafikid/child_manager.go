package main

import (
	"encoding/json"
	"sync"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// connSub is a registered per-child subscriber: a connection and an optional
// event filter.
type connSub struct {
	conn   control.Connection
	filter protocol.SubscribeFilter
}

// childEntry holds a live child and its per-child subscriber list.
type childEntry struct {
	c    *child.Child
	subs []*connSub
}

// labeledSub is a label-filtered subscriber: it receives events from every
// child whose labels satisfy the AND-match (labels) and key-presence
// (hasLabel) filter.  Delivery is re-evaluated on each event so label
// changes take effect immediately without re-subscribing.
type labeledSub struct {
	conn     control.Connection
	labels   map[string]string
	hasLabel []string
	filter   *protocol.SubscribeFilter // optional event-type filter
}

// ChildManager owns all live *child.Child instances and the per-child,
// global, and label-filtered subscriber registries. All exported methods are
// safe for concurrent use.
type ChildManager struct {
	mu       sync.RWMutex
	children map[string]*childEntry

	globalMu   sync.Mutex
	globalSubs []*connSub // global subscribers (lifecycle events only)

	labeledMu   sync.Mutex
	labeledSubs []*labeledSub // label-filtered subscribers
}

func newChildManager() *ChildManager {
	return &ChildManager{
		children: make(map[string]*childEntry),
	}
}

// Add registers c under childID. Called after a successful child.Spawn.
func (cm *ChildManager) Add(childID string, c *child.Child) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.children[childID] = &childEntry{c: c}
}

// Get returns the live child for childID, or (nil, false) if not present.
func (cm *ChildManager) Get(childID string) (*child.Child, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	e, ok := cm.children[childID]
	if !ok {
		return nil, false
	}
	return e.c, true
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

// Subscribe adds conn as a subscriber for events from childID.
func (cm *ChildManager) Subscribe(childID string, conn control.Connection, filter protocol.SubscribeFilter) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	e, ok := cm.children[childID]
	if !ok {
		return
	}
	e.subs = append(e.subs, &connSub{conn: conn, filter: filter})
}

// Unsubscribe removes conn's subscription for childID.
func (cm *ChildManager) Unsubscribe(childID string, conn control.Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	e, ok := cm.children[childID]
	if !ok {
		return
	}
	for i, s := range e.subs {
		if s.conn == conn {
			e.subs = append(e.subs[:i], e.subs[i+1:]...)
			return
		}
	}
}

// GetSubscribers returns a snapshot of the per-child subscriber list for childID.
// Used by handleInterceptedSend to preserve subscriptions across a kill+resume cycle:
// monitorChild.Remove (called by handleChildExit after the process exits) clears the
// subscriber list, so callers must save it before initiating the kill.
func (cm *ChildManager) GetSubscribers(childID string) []*connSub {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	e, ok := cm.children[childID]
	if !ok {
		return nil
	}
	out := make([]*connSub, len(e.subs))
	copy(out, e.subs)
	return out
}

// RestoreSubscribers appends subs to the per-child subscriber list for childID.
// Called after a respawn to reattach connections that survived the kill+resume cycle.
func (cm *ChildManager) RestoreSubscribers(childID string, subs []*connSub) {
	if len(subs) == 0 {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	e, ok := cm.children[childID]
	if !ok {
		return
	}
	e.subs = append(e.subs, subs...)
}

// GlobalSubscribe adds conn as a global subscriber (lifecycle events only).
func (cm *ChildManager) GlobalSubscribe(conn control.Connection) {
	cm.globalMu.Lock()
	defer cm.globalMu.Unlock()
	cm.globalSubs = append(cm.globalSubs, &connSub{conn: conn})
}

// GlobalUnsubscribe removes conn's global subscription.
func (cm *ChildManager) GlobalUnsubscribe(conn control.Connection) {
	cm.globalMu.Lock()
	defer cm.globalMu.Unlock()
	for i, s := range cm.globalSubs {
		if s.conn == conn {
			cm.globalSubs = append(cm.globalSubs[:i], cm.globalSubs[i+1:]...)
			return
		}
	}
}

// DeliverToChild delivers frame to all per-child subscribers registered for
// childID, applying each subscriber's filter.
func (cm *ChildManager) DeliverToChild(childID string, frame []byte) {
	cm.mu.RLock()
	e, ok := cm.children[childID]
	if !ok {
		cm.mu.RUnlock()
		return
	}
	// Copy subscriber slice under lock to avoid holding the lock during delivery.
	subs := make([]*connSub, len(e.subs))
	copy(subs, e.subs)
	cm.mu.RUnlock()

	for _, s := range subs {
		if !eventPassesFilter(frame, s.filter) {
			continue
		}
		s.conn.Deliver(frame)
	}
}

// DeliverToGlobal delivers a lifecycle event frame to all global subscribers.
func (cm *ChildManager) DeliverToGlobal(frame []byte) {
	cm.globalMu.Lock()
	subs := make([]*connSub, len(cm.globalSubs))
	copy(subs, cm.globalSubs)
	cm.globalMu.Unlock()

	for _, s := range subs {
		s.conn.Deliver(frame)
	}
}

// ─── Label-filtered subscriber methods ───────────────────────────────────────

// RegisterLabeled adds conn as a label-filtered subscriber and returns the
// registration handle (used by UnregisterLabeled for explicit cleanup; the
// connection-close path uses RemoveLabeledSubsForConn instead).
func (cm *ChildManager) RegisterLabeled(conn control.Connection, labels map[string]string, hasLabel []string, filter *protocol.SubscribeFilter) *labeledSub {
	sub := &labeledSub{
		conn:     conn,
		labels:   labels,
		hasLabel: hasLabel,
		filter:   filter,
	}
	cm.labeledMu.Lock()
	cm.labeledSubs = append(cm.labeledSubs, sub)
	cm.labeledMu.Unlock()
	return sub
}

// UnregisterLabeled removes a specific labeled subscription by pointer
// identity. No-op if already removed.
func (cm *ChildManager) UnregisterLabeled(sub *labeledSub) {
	cm.labeledMu.Lock()
	defer cm.labeledMu.Unlock()
	for i, s := range cm.labeledSubs {
		if s == sub {
			cm.labeledSubs = append(cm.labeledSubs[:i], cm.labeledSubs[i+1:]...)
			return
		}
	}
}

// RemoveLabeledSubsForConn removes all labeled subscriptions associated with
// conn. Called from OnConnectionClose to prevent delivery to closed connections.
func (cm *ChildManager) RemoveLabeledSubsForConn(conn control.Connection) {
	cm.labeledMu.Lock()
	defer cm.labeledMu.Unlock()
	keep := cm.labeledSubs[:0]
	for _, s := range cm.labeledSubs {
		if s.conn != conn {
			keep = append(keep, s)
		}
	}
	cm.labeledSubs = keep
}

// DeliverToMatching delivers frame to all labeled subscribers whose label
// filter matches currentLabels and whose event filter (if present) passes for
// this frame.  The label match is evaluated on each call so label changes
// take effect at the next event without requiring re-subscription.
//
// Note: subscribers whose labels previously matched but no longer do simply
// stop receiving events — no synthetic "left filter" event is emitted (v1).
func (cm *ChildManager) DeliverToMatching(childID string, currentLabels map[string]string, frame []byte) {
	cm.labeledMu.Lock()
	subs := make([]*labeledSub, len(cm.labeledSubs))
	copy(subs, cm.labeledSubs)
	cm.labeledMu.Unlock()

	for _, s := range subs {
		if !matchesLabelFilter(currentLabels, s.labels, s.hasLabel) {
			continue
		}
		var filter protocol.SubscribeFilter
		if s.filter != nil {
			filter = *s.filter
		}
		if !eventPassesFilter(frame, filter) {
			continue
		}
		s.conn.Deliver(frame)
	}
}

// eventPassesFilter reports whether frame should be delivered to a subscriber
// with the given filter. Empty filter means pass everything. The resolution
// rule is: (profile members) ∪ include − exclude. For v1, profile is not
// expanded; only include/exclude lists are applied.
//
// For ctrl_event frames the filter is applied to the inner pi event's type
// (spec §7.1), so that include:["agent_start"] works as expected even though
// the wire frame type is "ctrl_event". ctrl_child_* lifecycle frames continue
// to be filtered by their top-level type.
func eventPassesFilter(frame []byte, f protocol.SubscribeFilter) bool {
	if len(f.Include) == 0 && len(f.Exclude) == 0 {
		return true
	}
	// Decode enough to get the top-level type and, for ctrl_event, the inner
	// event payload.
	var hdr struct {
		Type  string          `json:"type"`
		Event json.RawMessage `json:"event,omitempty"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return true // unparseable frame; pass through
	}

	// For ctrl_event frames, match on the inner pi event type so that
	// subscriber filters like include:["agent_start"] continue to work
	// after the wrapper was applied in monitorChild.
	effectiveType := hdr.Type
	if hdr.Type == protocol.TypeCtrlEvent && len(hdr.Event) > 0 {
		var inner struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(hdr.Event, &inner) == nil && inner.Type != "" {
			effectiveType = inner.Type
		}
	}

	for _, ex := range f.Exclude {
		if ex == effectiveType {
			return false
		}
	}
	if len(f.Include) > 0 {
		for _, inc := range f.Include {
			if inc == effectiveType {
				return true
			}
		}
		return false
	}
	return true
}
