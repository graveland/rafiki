// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
		{"all takes anything", eventlog.Subject{Scope: eventlog.ScopeAll}, statusEvent("c_anything", "idle"), true},
		{"selector narrows", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_mid", "idle"), true},
		{"selector excludes a non-match", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_leaf", "idle"), false},
		{"selector excludes an unlabelled child", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "owner=brent"}, statusEvent("c_unknown", "idle"), false},
		{"malformed selector excludes", eventlog.Subject{Scope: eventlog.ScopeAll, Selector: "os="}, statusEvent("c_mid", "idle"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := eventlog.Filter{Subject: tc.subj, Tier: eventlog.TierAll}
			if got := f.Match(tc.ev, ln); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

// A subject must never be resolved to a fixed id set at open: a child that did
// not exist when the subscription started is in scope the moment lineage says
// it is. This is the property stream.go:32 gets wrong today.
func TestSubtreeAdmitsAChildThatAppearsLater(t *testing.T) {
	ln := fakeLineage{depth: map[string]map[string]int{"c_root": {}}, labels: map[string]map[string]string{}}
	f := eventlog.Filter{
		Subject: eventlog.Subject{Scope: eventlog.ScopeSubtree, ChildID: "c_root"},
		Tier:    eventlog.TierAll,
	}
	ev := statusEvent("c_new", "spawning")
	if f.Match(ev, ln) {
		t.Fatal("c_new matched before lineage knew about it")
	}
	ln.depth["c_root"]["c_new"] = 1
	if !f.Match(ev, ln) {
		t.Fatal("c_new did not match after lineage learned about it; the subject was resolved at construction")
	}
}

func TestTypeFilter(t *testing.T) {
	ln := fakeLineage{}
	f := eventlog.Filter{
		Subject: eventlog.Subject{Scope: eventlog.ScopeAll},
		Tier:    eventlog.TierAll,
		Types:   []string{"agent_status"},
	}
	if !f.Match(statusEvent("c_1", "idle"), ln) {
		t.Error("agent_status excluded by a filter that names it")
	}
	turn := &rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}}
	if f.Match(turn, ln) {
		t.Error("turn_start admitted by a filter that names only agent_status")
	}
}
