// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// fakeLineage is a map-backed Lineage. depth[ancestor][candidate] = hops.
type fakeLineage struct {
	depth  map[string]map[string]int
	labels map[string]map[string]string
}

func (f fakeLineage) DescendantDepth(a, c string) int {
	if m, ok := f.depth[a]; ok {
		if d, ok := m[c]; ok {
			return d
		}
	}
	return -1
}
func (f fakeLineage) Labels(id string) (map[string]string, bool) {
	l, ok := f.labels[id]
	return l, ok
}

func statusEvent(childID, state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

func TestSubjectMatch(t *testing.T) {
	ln := fakeLineage{
		depth: map[string]map[string]int{
			"c_root": {"c_mid": 1, "c_leaf": 2},
		},
		labels: map[string]map[string]string{
			"c_mid":  {"owner": "brent"},
			"c_leaf": {"owner": "someone"},
		},
	}

	for _, tc := range []struct {
		name string
		subj eventlog.Subject
		ev   *rafikiv1.Event
		want bool
	}{
		{"child matches itself", eventlog.Subject{Scope: eventlog.ScopeChild, ChildID: "c_mid"}, statusEvent("c_mid", "idle"), true},
		{"child does not match a sibling", eventlog.Subject{Scope: eventlog.ScopeChild, ChildID: "c_mid"}, statusEvent("c_other", "idle"), false},
		{"subtree unbounded takes a grandchild", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"}, statusEvent("c_leaf", "idle"), true},
		{"subtree depth 1 rejects a grandchild", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", MaxDepth: 1}, statusEvent("c_leaf", "idle"), false},
		{"subtree depth 1 takes a child", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", MaxDepth: 1}, statusEvent("c_mid", "idle"), true},
		{"subtree excludes the ancestor itself", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"}, statusEvent("c_root", "idle"), false},
		{"subtree include_self takes the ancestor itself", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", IncludeSelf: true}, statusEvent("c_root", "idle"), true},
		{"subtree include_self still takes a descendant", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", IncludeSelf: true}, statusEvent("c_leaf", "idle"), true},
		{"subtree include_self still honours max_depth below the root", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", MaxDepth: 1, IncludeSelf: true}, statusEvent("c_leaf", "idle"), false},
		{"include_self does not widen a child subject", eventlog.Subject{Scope: eventlog.ScopeChild, ChildID: "c_mid", IncludeSelf: true}, statusEvent("c_other", "idle"), false},
		{"include_self still honours the narrowing selector", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_mid", IncludeSelf: true, Selector: "owner=someone"}, statusEvent("c_mid", "idle"), false},
		{"all takes anything", eventlog.Subject{Scope: eventlog.ScopeAll}, statusEvent("c_anything", "idle"), true},
		{"selector narrows", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_mid", "idle"), true},
		{"selector excludes a non-match", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_leaf", "idle"), false},
		{"selector excludes an unlabelled child", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_unknown", "idle"), false},
		{"malformed selector excludes", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "os="}, statusEvent("c_mid", "idle"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := eventlog.Filter{Subject: tc.subj, Tier: eventlog.TierAll}
			assert.NewCollecting(t).Eq(tc.want, f.Match(tc.ev, ln), "Match")
		})
	}
}

// A subject must never be resolved to a fixed id set at open: a child that did
// not exist when the subscription started is in scope the moment lineage says
// it is. This is the property stream.go:32 gets wrong today.
func TestSubtreeAdmitsAChildThatAppearsLater(t *testing.T) {
	c := assert.NewAborting(t)
	ln := fakeLineage{depth: map[string]map[string]int{"c_root": {}}, labels: map[string]map[string]string{}}
	f := eventlog.Filter{
		Subject: eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"},
		Tier:    eventlog.TierAll,
	}
	ev := statusEvent("c_new", "spawning")
	c.False(f.Match(ev, ln), "c_new matched before lineage knew about it")
	ln.depth["c_root"]["c_new"] = 1
	c.True(f.Match(ev, ln), "c_new did not match after lineage learned about it; the subject was resolved at construction")
}

func TestTypeFilter(t *testing.T) {
	c := assert.NewCollecting(t)
	ln := fakeLineage{}
	f := eventlog.Filter{
		Subject: eventlog.Subject{Scope: eventlog.ScopeAll},
		Tier:    eventlog.TierAll,
		Types:   []string{"agent_status"},
	}
	c.True(f.Match(statusEvent("c_1", "idle"), ln), "agent_status excluded by a filter that names it")
	turn := &rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}}
	c.False(f.Match(turn, ln), "turn_start admitted by a filter that names only agent_status")
}

// closedEvent is what the daemon publishes when a child leaves its store: the
// envelope names the child, and the payload carries the chain and labels,
// because the lineage can no longer answer for it.
func closedEvent(childID string, ancestors []string, labels map[string]string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_ChildClosed{ChildClosed: &rafikiv1.ChildClosed{
			Ancestors: ancestors,
			Labels:    labels,
		}},
	}
}

// A closed child is GONE from the store the lineage reads, so the one
// subscriber that is still drawing its row -- one scoped to an ancestor --
// could never be told. The event carries its own chain, and the scope question
// is answered from that instead.
func TestMatchAdmitsAClosedChildFromItsOwnLineage(t *testing.T) {
	// The store has already forgotten c_leaf; only c_root remains.
	ln := fakeLineage{
		depth:  map[string]map[string]int{"c_root": {"c_mid": 1}},
		labels: map[string]map[string]string{"c_mid": {"owner": "brent"}},
	}
	ev := closedEvent("c_leaf", []string{"c_mid", "c_root"}, map[string]string{"owner": "someone"})

	for _, tc := range []struct {
		name string
		subj eventlog.Subject
		want bool
	}{
		{"an ancestor's subtree still hears about it", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"}, true},
		{"the immediate parent's subtree hears about it", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_mid"}, true},
		{"a depth-1 ancestor takes a child of its own", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_mid", MaxDepth: 1}, true},
		{"a depth-1 ancestor rejects a grandchild", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root", MaxDepth: 1}, false},
		{"an unrelated subtree does not", eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_other"}, false},
		{"a sibling's child subject does not", eventlog.Subject{Scope: eventlog.ScopeChild, ChildID: "c_mid"}, false},
		{"the child's own subject does", eventlog.Subject{Scope: eventlog.ScopeChild, ChildID: "c_leaf"}, true},
		{"all takes it", eventlog.Subject{Scope: eventlog.ScopeAll}, true},
		{"a selector reads the carried labels", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=someone"}, true},
		{"a selector that does not match excludes", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, false},
		{"a malformed selector still excludes", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "os="}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := eventlog.Filter{Subject: tc.subj, Tier: eventlog.TierAll}
			assert.NewCollecting(t).Eq(tc.want, f.Match(ev, ln), "Match")
		})
	}
}

// The fallback is for the close event ONLY. Every other event is about a child
// that is still in the store, and an id the lineage does not know stays
// excluded -- otherwise any client could widen its own subscription by naming
// an arbitrary child.
func TestMatchDoesNotUseTheFallbackForALivingChild(t *testing.T) {
	c := assert.NewCollecting(t)
	ln := fakeLineage{depth: map[string]map[string]int{"c_root": {}}, labels: map[string]map[string]string{}}
	f := eventlog.Filter{
		Subject: eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"},
		Tier:    eventlog.TierAll,
	}
	// An agent_status that happens to carry the same lineage fields is still
	// judged by the store alone: the payload is not a close.
	ev := statusEvent("c_unknown", "idle")
	c.False(f.Match(ev, ln), "a non-close event for an unknown child was admitted")
}
