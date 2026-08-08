package main

import (
	"encoding/json"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── fake Connection ──────────────────────────────────────────────────────────

// collectConn is a control.Connection that collects all delivered frames.
type collectConn struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *collectConn) Deliver(frame []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, frame)
}

func (c *collectConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// Verify collectConn satisfies the interface.
var _ control.Connection = (*collectConn)(nil)

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeTypedFrame(typ, childID string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "childId": childID})
	return b
}

func makeCtrlEventFrame(childID, innerType string) []byte {
	inner, _ := json.Marshal(map[string]string{"type": innerType})
	env := protocol.CtrlEvent{
		Type:    protocol.TypeCtrlEvent,
		ChildID: childID,
		Event:   json.RawMessage(inner),
	}
	b, _ := json.Marshal(env)
	return b
}

// ─── RegisterLabeled / UnregisterLabeled ──────────────────────────────────────

func TestRegisterLabeled_AppearsInList(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}

	sub := cm.RegisterLabeled(conn, map[string]string{"env": "prod"}, nil, nil)
	if sub == nil {
		t.Fatal("RegisterLabeled returned nil")
	}

	cm.labeledMu.Lock()
	n := len(cm.labeledSubs)
	cm.labeledMu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 labeled sub, got %d", n)
	}
}

func TestUnregisterLabeled_RemovesEntry(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}

	sub1 := cm.RegisterLabeled(conn, map[string]string{"a": "1"}, nil, nil)
	sub2 := cm.RegisterLabeled(conn, map[string]string{"b": "2"}, nil, nil)

	cm.UnregisterLabeled(sub1)

	cm.labeledMu.Lock()
	remaining := make([]*labeledSub, len(cm.labeledSubs))
	copy(remaining, cm.labeledSubs)
	cm.labeledMu.Unlock()

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining))
	}
	if remaining[0] != sub2 {
		t.Error("wrong sub remaining after unregister")
	}
}

func TestUnregisterLabeled_NoopIfAlreadyRemoved(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	sub := cm.RegisterLabeled(conn, map[string]string{"x": "y"}, nil, nil)
	cm.UnregisterLabeled(sub)
	// Second call must not panic.
	cm.UnregisterLabeled(sub)

	cm.labeledMu.Lock()
	n := len(cm.labeledSubs)
	cm.labeledMu.Unlock()
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

// ─── RemoveLabeledSubsForConn ─────────────────────────────────────────────────

func TestRemoveLabeledSubsForConn_RemovesAll(t *testing.T) {
	cm := newChildManager()
	connA := &collectConn{}
	connB := &collectConn{}

	cm.RegisterLabeled(connA, map[string]string{"x": "1"}, nil, nil)
	cm.RegisterLabeled(connA, map[string]string{"x": "2"}, nil, nil)
	cm.RegisterLabeled(connB, map[string]string{"y": "3"}, nil, nil)

	cm.RemoveLabeledSubsForConn(connA)

	cm.labeledMu.Lock()
	remaining := make([]*labeledSub, len(cm.labeledSubs))
	copy(remaining, cm.labeledSubs)
	cm.labeledMu.Unlock()

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining (connB's sub), got %d", len(remaining))
	}
	if remaining[0].conn != connB {
		t.Error("wrong sub remaining")
	}
}

func TestRemoveLabeledSubsForConn_NoopIfNone(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	// Must not panic when conn has no labeled subs.
	cm.RemoveLabeledSubsForConn(conn)
}

// ─── DeliverToMatching ────────────────────────────────────────────────────────

func TestDeliverToMatching_MatchingLabelReceivesFrame(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, map[string]string{"env": "prod"}, nil, nil)

	frame := makeTypedFrame(protocol.TypeCtrlChildStatus, "c_001")
	cm.DeliverToMatching("c_001", map[string]string{"env": "prod"}, frame)

	if conn.count() != 1 {
		t.Errorf("expected 1 frame delivered, got %d", conn.count())
	}
}

func TestDeliverToMatching_NonMatchingLabelSkipped(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, map[string]string{"env": "prod"}, nil, nil)

	frame := makeTypedFrame(protocol.TypeCtrlChildStatus, "c_001")
	// Child has env=staging, not env=prod.
	cm.DeliverToMatching("c_001", map[string]string{"env": "staging"}, frame)

	if conn.count() != 0 {
		t.Errorf("expected 0 frames delivered, got %d", conn.count())
	}
}

func TestDeliverToMatching_HasLabelMatches(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, nil, []string{"tier"}, nil)

	frame := makeTypedFrame(protocol.TypeCtrlChildStatus, "c_001")
	cm.DeliverToMatching("c_001", map[string]string{"tier": "fast", "env": "prod"}, frame)

	if conn.count() != 1 {
		t.Errorf("expected 1 frame, got %d", conn.count())
	}
}

func TestDeliverToMatching_HasLabelMissing_Skipped(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, nil, []string{"tier"}, nil)

	frame := makeTypedFrame(protocol.TypeCtrlChildStatus, "c_001")
	// Labels don't have "tier".
	cm.DeliverToMatching("c_001", map[string]string{"env": "prod"}, frame)

	if conn.count() != 0 {
		t.Errorf("expected 0 frames, got %d", conn.count())
	}
}

func TestDeliverToMatching_MultipleSubscribers_IndependentFilters(t *testing.T) {
	cm := newChildManager()
	connWork := &collectConn{}
	connPersonal := &collectConn{}

	cm.RegisterLabeled(connWork, map[string]string{"context": "work"}, nil, nil)
	cm.RegisterLabeled(connPersonal, map[string]string{"context": "personal"}, nil, nil)

	frameWork := makeTypedFrame(protocol.TypeCtrlEvent, "c_work")
	framePersonal := makeTypedFrame(protocol.TypeCtrlEvent, "c_personal")

	cm.DeliverToMatching("c_work", map[string]string{"context": "work"}, frameWork)
	cm.DeliverToMatching("c_personal", map[string]string{"context": "personal"}, framePersonal)

	if connWork.count() != 1 {
		t.Errorf("connWork: expected 1, got %d", connWork.count())
	}
	if connPersonal.count() != 1 {
		t.Errorf("connPersonal: expected 1, got %d", connPersonal.count())
	}
}

func TestDeliverToMatching_NoLabels_MatchesAll(t *testing.T) {
	// A subscriber with empty labels/hasLabel matches every child.
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, nil, nil, nil)

	frame := makeTypedFrame(protocol.TypeCtrlChildStatus, "c_001")
	cm.DeliverToMatching("c_001", map[string]string{"env": "prod"}, frame)
	cm.DeliverToMatching("c_002", map[string]string{"env": "staging"}, frame)

	if conn.count() != 2 {
		t.Errorf("expected 2 frames, got %d", conn.count())
	}
}

func TestDeliverToMatching_EventFilter_Include(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	fp := &protocol.SubscribeFilter{Include: []string{"agent_start"}}
	cm.RegisterLabeled(conn, map[string]string{"env": "prod"}, nil, fp)

	labels := map[string]string{"env": "prod"}

	// Matching event type: agent_start inside ctrl_event envelope.
	cm.DeliverToMatching("c_001", labels, makeCtrlEventFrame("c_001", "agent_start"))
	// Non-matching event type.
	cm.DeliverToMatching("c_001", labels, makeCtrlEventFrame("c_001", "agent_end"))

	if conn.count() != 1 {
		t.Errorf("expected 1 frame (agent_start only), got %d", conn.count())
	}
}

func TestDeliverToMatching_EventFilter_Exclude(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	fp := &protocol.SubscribeFilter{Exclude: []string{"message_update"}}
	cm.RegisterLabeled(conn, map[string]string{"env": "prod"}, nil, fp)

	labels := map[string]string{"env": "prod"}

	cm.DeliverToMatching("c_001", labels, makeCtrlEventFrame("c_001", "agent_start"))
	cm.DeliverToMatching("c_001", labels, makeCtrlEventFrame("c_001", "message_update"))
	cm.DeliverToMatching("c_001", labels, makeCtrlEventFrame("c_001", "agent_end"))

	if conn.count() != 2 {
		t.Errorf("expected 2 frames (excluding message_update), got %d", conn.count())
	}
}

// ─── Dynamic label matching ───────────────────────────────────────────────────

// TestDeliverToMatching_LabelChangeStartsDelivery verifies that once a child's
// labels change to match a filter, the subscriber starts receiving events
// without requiring re-subscription.
func TestDeliverToMatching_LabelChangeStartsDelivery(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, map[string]string{"context": "work"}, nil, nil)

	frame := makeTypedFrame(protocol.TypeCtrlEvent, "c_001")

	// Before label change: no match.
	cm.DeliverToMatching("c_001", map[string]string{}, frame)
	if conn.count() != 0 {
		t.Fatalf("expected 0 before label change, got %d", conn.count())
	}

	// After label change: matches → events flow.
	cm.DeliverToMatching("c_001", map[string]string{"context": "work"}, frame)
	if conn.count() != 1 {
		t.Errorf("expected 1 after label change, got %d", conn.count())
	}
}

// TestDeliverToMatching_LabelChangeStopsDelivery verifies that when a child's
// labels change away from a match, the subscriber stops receiving events.
// No synthetic "left filter" event is emitted (v1 simplification).
func TestDeliverToMatching_LabelChangeStopsDelivery(t *testing.T) {
	cm := newChildManager()
	conn := &collectConn{}
	cm.RegisterLabeled(conn, map[string]string{"context": "work"}, nil, nil)

	frame := makeTypedFrame(protocol.TypeCtrlEvent, "c_001")

	// Matching.
	cm.DeliverToMatching("c_001", map[string]string{"context": "work"}, frame)
	if conn.count() != 1 {
		t.Fatalf("expected 1 before label change, got %d", conn.count())
	}

	// After label change away from match.
	cm.DeliverToMatching("c_001", map[string]string{"context": "personal"}, frame)
	if conn.count() != 1 {
		t.Errorf("expected still 1 after label change, got %d", conn.count())
	}
}

// ─── Connection-close cleanup ─────────────────────────────────────────────────

func TestConnectionClose_CleansUpLabeledSubs(t *testing.T) {
	st := childstore.New()
	ctrl := NewController(st, t.TempDir(), t.TempDir(), "/tmp/test.sock", nil, nil, nil, t.Context())

	conn := &collectConn{}
	if err := ctrl.SubscribeLabeled(conn, map[string]string{"env": "prod"}, nil, protocol.SubscribeFilter{}); err != nil {
		t.Fatalf("SubscribeLabeled: %v", err)
	}

	ctrl.cm.labeledMu.Lock()
	before := len(ctrl.cm.labeledSubs)
	ctrl.cm.labeledMu.Unlock()

	if before != 1 {
		t.Fatalf("expected 1 labeled sub before close, got %d", before)
	}

	ctrl.OnConnectionClose(conn)

	ctrl.cm.labeledMu.Lock()
	after := len(ctrl.cm.labeledSubs)
	ctrl.cm.labeledMu.Unlock()

	if after != 0 {
		t.Errorf("expected 0 labeled subs after connection close, got %d", after)
	}
}

// TestSubscribeLabeled_NilFilterStored verifies that a zero-value filter is
// stored as nil (no unnecessary allocation) by SubscribeLabeled.
func TestSubscribeLabeled_NilFilterStored(t *testing.T) {
	st := childstore.New()
	ctrl := NewController(st, t.TempDir(), t.TempDir(), "/tmp/test.sock", nil, nil, nil, t.Context())
	conn := &collectConn{}

	if err := ctrl.SubscribeLabeled(conn, map[string]string{"x": "y"}, nil, protocol.SubscribeFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctrl.cm.labeledMu.Lock()
	subs := make([]*labeledSub, len(ctrl.cm.labeledSubs))
	copy(subs, ctrl.cm.labeledSubs)
	ctrl.cm.labeledMu.Unlock()

	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d", len(subs))
	}
	if subs[0].filter != nil {
		t.Error("expected nil filter for empty SubscribeFilter")
	}
}

// TestSubscribeLabeled_NonEmptyFilterStored verifies that a non-empty filter is
// preserved as a non-nil pointer.
func TestSubscribeLabeled_NonEmptyFilterStored(t *testing.T) {
	st := childstore.New()
	ctrl := NewController(st, t.TempDir(), t.TempDir(), "/tmp/test.sock", nil, nil, nil, t.Context())
	conn := &collectConn{}

	f := protocol.SubscribeFilter{Include: []string{"agent_start"}}
	if err := ctrl.SubscribeLabeled(conn, map[string]string{"x": "y"}, nil, f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctrl.cm.labeledMu.Lock()
	subs := make([]*labeledSub, len(ctrl.cm.labeledSubs))
	copy(subs, ctrl.cm.labeledSubs)
	ctrl.cm.labeledMu.Unlock()

	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d", len(subs))
	}
	if subs[0].filter == nil {
		t.Fatal("expected non-nil filter")
	}
	if len(subs[0].filter.Include) != 1 || subs[0].filter.Include[0] != "agent_start" {
		t.Errorf("filter.Include: %v", subs[0].filter.Include)
	}
}
