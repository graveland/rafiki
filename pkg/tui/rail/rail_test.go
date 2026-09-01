// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

func summary(id, name, parent, status string, latest int32) *rafikiv1.ChildSummary {
	s := &rafikiv1.ChildSummary{
		ChildId:       id,
		Name:          name,
		Status:        status,
		Labels:        map[string]string{},
		LatestOrdinal: &latest,
	}
	if parent != "" {
		s.Labels[rail.ParentLabel] = parent
	}
	return s
}

func spawned(id, parent, name string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId: id, ParentId: parent, Name: name,
		}},
	}
}

func exited(id string, code, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{
			ChildId: id, ExitCode: &code,
		}},
	}
}

func TestSeedSkipsExitedChildren(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_live", "coordinator", "", "idle", 3),
		summary("c_dead", "old worker", "", "exited", 99),
	})
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 -- a historical exited child must never be seeded", r.Len())
	}
	if _, ok := r.Get("c_dead"); ok {
		t.Error("c_dead must not be in the rail")
	}
}

func TestSeedAcceptsEveryLiveStatus(t *testing.T) {
	var sums []*rafikiv1.ChildSummary
	for i, st := range rail.LiveStatuses() {
		if st == "running" {
			t.Fatal(`"running" is not a protocol.Status value -- the set of eight is closed`)
		}
		sums = append(sums, summary("c_"+st, st, "", st, int32(i)))
	}
	r := rail.New()
	r.Seed(sums)
	if r.Len() != 7 {
		t.Fatalf("Len = %d, want 7; LiveStatuses = %v", r.Len(), rail.LiveStatuses())
	}
}

func TestChildSpawnedAddsARow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_root", "coordinator", "", "idle", 0)})
	r.Apply(spawned("c_kid", "c_root", "scout", 0))

	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
	n, ok := r.Get("c_kid")
	if !ok {
		t.Fatal("c_kid missing")
	}
	if n.ParentID != "c_root" || n.Name != "scout" {
		t.Errorf("node = %+v, want parent c_root name scout", n)
	}
}

func TestChildExitedKeepsTheRow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_kid", "scout", "", "idle", 0)})
	r.Apply(exited("c_kid", 0, 1))

	n, ok := r.Get("c_kid")
	if !ok {
		t.Fatal("an exited child's row must be KEPT for the session -- dropping it deletes " +
			"the row out from under a reader at the moment its output became final")
	}
	if !n.Exited || n.ExitCode == nil || *n.ExitCode != 0 {
		t.Errorf("node = %+v, want exited with code 0", n)
	}
}

func TestDisplayOrderIsParentThenChildren(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_root", "coordinator", "", "idle", 0),
		summary("c_b", "builder", "c_root", "idle", 0),
		summary("c_a", "scout", "c_root", "idle", 0),
	})
	var got []string
	for _, n := range r.Nodes() {
		got = append(got, n.ChildID)
	}
	// Siblings sort by NAME: builder before scout.
	want := []string{"c_root", "c_b", "c_a"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if d, _ := r.Get("c_a"); d.Depth != 1 {
		t.Errorf("c_a depth = %d, want 1", d.Depth)
	}
}

func TestEventForAnUnknownChildIsIgnored(t *testing.T) {
	r := rail.New()
	r.Apply(exited("c_ghost", 1, 0))
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0 -- only child_spawned may introduce a row", r.Len())
	}
}

// A reconnect re-seeds to discover children spawned during the disconnect,
// whose child_spawned is in the past. That must not wipe reading history.
func TestReSeedPreservesWatermarkAndBadge(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_1", "scout", "", "idle", 0)})
	r.Apply(&rafikiv1.Event{ChildId: "c_1", Ordinal: ptr(int32(4)),
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}})
	if n, _ := r.Get("c_1"); n.Attention != 1 {
		t.Fatalf("attention = %d, want 1 before the re-seed", n.Attention)
	}

	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_1", "scout", "", "idle", 9),
		summary("c_2", "builder", "c_1", "idle", 0),
	})

	n, _ := r.Get("c_1")
	if n.Attention != 1 {
		t.Errorf("attention = %d, want 1 -- a re-seed is not a read", n.Attention)
	}
	if n.Seen != 0 {
		t.Errorf("seen = %d, want 0 -- re-seeding must not advance the watermark", n.Seen)
	}
	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2 -- the re-seed must discover c_2", r.Len())
	}
}

func ptr[T any](v T) *T { return &v }

// costTurnEnd is a turn_end carrying a cost.
func costTurnEnd(childID string, ordinal int32, cost float64) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Ordinal: &ordinal,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{CostUsd: &cost}},
	}
}

// TurnEnd carries the cost of ONE turn (Emitter.AgentEnd resets its usage), so
// costs are summed.
func TestRailSumsTurnCost(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 0, 0.25))
	r.Apply(costTurnEnd("c1", 1, 0.75))

	n, ok := r.Get("c1")
	if !ok {
		t.Fatal("c1 missing")
	}
	if n.Cost != 1.0 {
		t.Errorf("Cost = %v, want 1.0", n.Cost)
	}
}

// The rail and focus subscriptions overlap on the durable tier, so the same
// turn_end arrives twice. Summing without a watermark doubles the bill.
func TestRailDoesNotDoubleCountADuplicateTurnEnd(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 7, 0.50))
	r.Apply(costTurnEnd("c1", 7, 0.50))

	n, _ := r.Get("c1")
	if n.Cost != 0.50 {
		t.Errorf("Cost = %v, want 0.50: a duplicate ordinal must not be counted twice", n.Cost)
	}
}

// A focused agent's headline number includes what its subagents spent.
func TestSubtreeCostSumsDescendants(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(spawned("c2", "c1", "worker", 0))
	r.Apply(spawned("c3", "c2", "deep", 0))
	r.Apply(costTurnEnd("c1", 0, 1.0))
	r.Apply(costTurnEnd("c2", 0, 2.0))
	r.Apply(costTurnEnd("c3", 0, 4.0))

	if got := r.SubtreeCost("c1"); got != 7.0 {
		t.Errorf("SubtreeCost(c1) = %v, want 7.0", got)
	}
	if got := r.SubtreeCost("c2"); got != 6.0 {
		t.Errorf("SubtreeCost(c2) = %v, want 6.0", got)
	}
	if got := r.SubtreeCost("c3"); got != 4.0 {
		t.Errorf("SubtreeCost(c3) = %v, want 4.0", got)
	}
}

// A seed from ListChildren must not be undone by a later stream event, and
// must not be added to twice.
func TestSetCostSeedsWithoutDoubleCounting(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.SetCost("c1", 5.0)
	r.SetCost("c1", 5.0)
	n, _ := r.Get("c1")
	if n.Cost != 5.0 {
		t.Errorf("Cost = %v, want 5.0: SetCost assigns, it does not add", n.Cost)
	}
}
