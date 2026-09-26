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
			t.Fatal(`"running" is not a protocol.Status value -- the set of nine is closed`)
		}
		sums = append(sums, summary("c_"+st, st, "", st, int32(i)))
	}
	r := rail.New()
	r.Seed(sums)
	if r.Len() != 8 {
		t.Fatalf("Len = %d, want 8; LiveStatuses = %v", r.Len(), rail.LiveStatuses())
	}
}

func TestSeedPopulatesCwd(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Cwd: "/work/scout", Labels: map[string]string{}},
	})
	n, ok := r.Get("c_1")
	if !ok {
		t.Fatal("c_1 not seeded")
	}
	if n.Cwd != "/work/scout" {
		t.Errorf("Cwd = %q, want /work/scout", n.Cwd)
	}

	// A re-seed refreshes Cwd the same way it refreshes Name and SessionID --
	// it is daemon-authoritative, not client reading history.
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Cwd: "/work/scout-renamed", Labels: map[string]string{}},
	})
	n, _ = r.Get("c_1")
	if n.Cwd != "/work/scout-renamed" {
		t.Errorf("Cwd after re-seed = %q, want /work/scout-renamed", n.Cwd)
	}
}

func TestSeedPopulatesMaxCost(t *testing.T) {
	cap1 := 5.0
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &cap1},
		{ChildId: "c_2", Name: "uncapped", Status: "idle", Labels: map[string]string{}},
	})
	n, ok := r.Get("c_1")
	if !ok {
		t.Fatal("c_1 not seeded")
	}
	if n.MaxCost != 5.0 {
		t.Errorf("MaxCost = %v, want 5.0", n.MaxCost)
	}
	n2, ok := r.Get("c_2")
	if !ok {
		t.Fatal("c_2 not seeded")
	}
	if n2.MaxCost != 0 {
		t.Errorf("MaxCost = %v, want 0 (no cap set)", n2.MaxCost)
	}

	// A re-seed refreshes MaxCost the same way it refreshes Cwd/SessionID --
	// daemon-authoritative, not client reading history.
	cap2 := 10.0
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &cap2},
	})
	n, _ = r.Get("c_1")
	if n.MaxCost != 10.0 {
		t.Errorf("MaxCost after re-seed = %v, want 10.0", n.MaxCost)
	}
}

func TestSeedCarriesKind(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_a", Name: "worker", Kind: "claude", Status: "idle"},
		{ChildId: "c_b", Name: "native", Kind: "fundi", Status: "idle"},
	})
	for _, tc := range []struct{ id, want string }{{"c_a", "claude"}, {"c_b", "fundi"}} {
		n, ok := r.Get(tc.id)
		if !ok {
			t.Fatalf("no node for %q", tc.id)
		}
		if n.Kind != tc.want {
			t.Errorf("Node(%q).Kind = %q, want %q", tc.id, n.Kind, tc.want)
		}
	}
	// Re-seed refresh: a node introduced by child_spawned arrives Kind-less and
	// only a later Seed fills it, so the refresh path must update Kind too.
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_a", Name: "worker", Kind: "fundi", Status: "idle"},
	})
	if n, ok := r.Get("c_a"); !ok || n.Kind != "fundi" {
		t.Errorf("Kind after re-seed = %q (ok=%v), want fundi", n.Kind, ok)
	}
}

func TestSetMaxCostAssignsDirectly(t *testing.T) {
	r := rail.New()
	capVal := 10.0
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &capVal},
	})

	// Raising MaxCost
	r.SetMaxCost("c_1", 20.0)
	n, ok := r.Get("c_1")
	if !ok || n.MaxCost != 20.0 {
		t.Fatalf("MaxCost = %v, want 20.0", n.MaxCost)
	}

	// Lowering MaxCost (must actually lower, unlike SetCost which takes max)
	r.SetMaxCost("c_1", 5.0)
	n, _ = r.Get("c_1")
	if n.MaxCost != 5.0 {
		t.Fatalf("MaxCost = %v, want 5.0 (lowered)", n.MaxCost)
	}

	// Clearing MaxCost (setting to 0)
	r.SetMaxCost("c_1", 0)
	n, _ = r.Get("c_1")
	if n.MaxCost != 0 {
		t.Fatalf("MaxCost = %v, want 0 (cleared)", n.MaxCost)
	}

	// Unknown child ID is a silent no-op
	r.SetMaxCost("c_unknown", 100.0)
}

func TestWorkingMatchesTheMidTurnStatuses(t *testing.T) {
	for _, st := range []string{"streaming", "tool_running", "compacting"} {
		if !rail.Working(st) {
			t.Errorf("Working(%q) = false, want true", st)
		}
	}
	for _, st := range []string{"spawning", "idle", "blocked_ui", "shutting_down", "exited", "running", ""} {
		if rail.Working(st) {
			t.Errorf("Working(%q) = true, want false", st)
		}
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

// assistantMessageWithCost is an assistant_message carrying a running
// in-flight cost total.
func assistantMessageWithCost(childID string, ordinal int32, cost float64) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Ordinal: &ordinal,
		Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{CostUsd: &cost}},
	}
}

// turnEndWithUsage is a turn_end carrying token usage but no cost, for
// exercising CtxTokens/CtxThrough independently of the cost fold.
func turnEndWithUsage(childID string, ordinal int32, input, cacheRead, cacheWrite int64) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Ordinal: &ordinal,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{Usage: &rafikiv1.Usage{
			InputTokens:      &input,
			CacheReadTokens:  &cacheRead,
			CacheWriteTokens: &cacheWrite,
		}}},
	}
}

// turnEndWithUsageNoOrdinal is turnEndWithUsage but without an ordinal --
// the unreproducible-event case the reading/bill distinction admits.
func turnEndWithUsageNoOrdinal(childID string, input, cacheRead, cacheWrite int64) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{Usage: &rafikiv1.Usage{
			InputTokens:      &input,
			CacheReadTokens:  &cacheRead,
			CacheWriteTokens: &cacheWrite,
		}}},
	}
}

func TestLiveCostMovesOnEveryReplyWithinATurn(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))

	r.Apply(assistantMessageWithCost("c1", 1, 0.10))
	if n, _ := r.Get("c1"); n.TotalCost() != 0.10 {
		t.Errorf("TotalCost after 1 reply = %v, want 0.10", n.TotalCost())
	}

	r.Apply(assistantMessageWithCost("c1", 2, 0.25))
	if n, _ := r.Get("c1"); n.TotalCost() != 0.25 {
		t.Errorf("TotalCost after 2nd reply = %v, want 0.25 (running total, not a sum)", n.TotalCost())
	}
}

// TurnEnd folds the live number into the settled total and resets the live
// counter, so the NEXT turn's first reply does not start from the previous
// turn's leftover running total.
func TestTurnEndFoldsLiveCostAndResetsIt(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(assistantMessageWithCost("c1", 1, 0.50))
	r.Apply(costTurnEnd("c1", 2, 0.60)) // the turn's own final total, slightly
	// different from the last live reading
	// (pricing can differ per-component
	// between a partial and final usage)

	n, _ := r.Get("c1")
	if n.Cost != 0.60 {
		t.Errorf("Cost = %v, want 0.60 (the settled TurnEnd total)", n.Cost)
	}
	if n.CostLive != 0 {
		t.Errorf("CostLive = %v, want 0 (reset once the turn settled)", n.CostLive)
	}
	if n.TotalCost() != 0.60 {
		t.Errorf("TotalCost = %v, want 0.60", n.TotalCost())
	}

	// A second turn's first reply starts its own running total.
	r.Apply(assistantMessageWithCost("c1", 3, 0.05))
	n, _ = r.Get("c1")
	if n.TotalCost() != 0.65 {
		t.Errorf("TotalCost after 2nd turn's 1st reply = %v, want 0.65 (0.60 settled + 0.05 live)", n.TotalCost())
	}
}

func TestSubtreeCostIncludesLiveCost(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(spawned("c2", "c1", "worker", 0))
	r.Apply(costTurnEnd("c1", 0, 1.0))
	r.Apply(assistantMessageWithCost("c2", 1, 0.30))

	if got := r.SubtreeCost("c1"); got != 1.30 {
		t.Errorf("SubtreeCost(c1) = %v, want 1.30 (1.0 settled + 0.30 live worker)", got)
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

// A re-seed carrying a rollup computed before the newest conversation_turn
// rows were visible must not erase cost this rail already counted: those
// ordinals are below CostThrough and Apply will never re-add them.
func TestSetCostNeverLowersAnAccumulatedTotal(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 0, 3.0))
	r.SetCost("c1", 1.0) // a lagging server rollup

	n, _ := r.Get("c1")
	if n.Cost != 3.0 {
		t.Errorf("Cost = %v, want 3.0: a stale seed must not lower the total", n.Cost)
	}

	// The seed IS authoritative for turns that predate this client, which the
	// stream never replays -- those only ever raise the number.
	r.SetCost("c1", 9.0)
	n, _ = r.Get("c1")
	if n.Cost != 9.0 {
		t.Errorf("Cost = %v, want 9.0: a higher seed must win", n.Cost)
	}
}

// statusEvt is an agent_status carrying a status at a given ordinal.
func statusEvt(childID string, ordinal int32, state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Ordinal: &ordinal,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

// The rail and focus subscriptions overlap on the durable tier with no
// ordering between the two goroutines that deliver them, so an older
// agent_status can arrive after a newer one. Without an ordinal gate the
// glyph/spinner would flip backwards until the newer state re-arrives --
// visibly out of sync with reality.
func TestApplyIgnoresAnOlderStatusArrivingAfterANewerOne(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(statusEvt("c1", 5, "idle"))
	r.Apply(statusEvt("c1", 2, "streaming")) // stale, delivered late by the other feed

	n, _ := r.Get("c1")
	if n.Status != "idle" {
		t.Errorf("Status = %q, want idle: a lower ordinal must not overwrite a higher one", n.Status)
	}
}

// A ChildExited must not be undone by a stale AgentStatus that outraces it
// through the other feed.
func TestApplyIgnoresAStaleStatusArrivingAfterExit(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(exited("c1", 0, 10))
	r.Apply(statusEvt("c1", 3, "tool_running"))

	n, _ := r.Get("c1")
	if !n.Exited {
		t.Error("Exited = false, want true: a stale status must not un-exit the row")
	}
	if n.Status != "exited" {
		t.Errorf("Status = %q, want exited", n.Status)
	}
}

func TestRemoveDropsTheNodeAndClearsFocus(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha", Status: "idle"},
		{ChildId: "c_2", Name: "beta", Status: "idle"},
	})
	r.SetFocus("c_1")

	r.Remove("c_1")

	if _, ok := r.Get("c_1"); ok {
		t.Error("removed node is still present")
	}
	if r.Focus() != "" {
		t.Errorf("Focus() = %q, want cleared when the focused node is removed", r.Focus())
	}
	if _, ok := r.Get("c_2"); !ok {
		t.Error("Remove took an unrelated node with it")
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{{ChildId: "c_1", Status: "idle"}})
	r.Remove("c_1")
	r.Remove("c_1") // must not panic
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
}

// turn_end usage sets CtxTokens to the prompt size the NEXT call will carry:
// input + cache-read + cache-write, and records the watermark it landed at.
func TestTurnEndUsageSetsCtxTokens(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 1, 10_000, 50_000, 2_000))

	n, _ := r.Get("c1")
	if n.CtxTokens != 62_000 {
		t.Errorf("CtxTokens = %d, want 62000 (input+cache_read+cache_write)", n.CtxTokens)
	}
	if n.CtxThrough != 1 {
		t.Errorf("CtxThrough = %d, want 1", n.CtxThrough)
	}
}

// The rail and focus subscriptions overlap on the durable tier, so an older
// turn_end can arrive after a newer one already landed. The watermark must
// stop it from regressing the displayed figure, same as CostThrough.
func TestTurnEndUsageReplayDoesNotRegressCtxTokens(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 5, 100_000, 0, 0))
	r.Apply(turnEndWithUsage("c1", 2, 1_000, 0, 0)) // stale replay, lower ordinal

	n, _ := r.Get("c1")
	if n.CtxTokens != 100_000 {
		t.Errorf("CtxTokens = %d, want 100000: a lower-ordinal replay must not regress it", n.CtxTokens)
	}
	if n.CtxThrough != 5 {
		t.Errorf("CtxThrough = %d, want 5", n.CtxThrough)
	}
}

// Unlike Cost, CtxTokens is a reading, not a running total -- ordinal 0 is
// both a legal ordinal and CtxThrough's zero value, so the child's very
// first turn_end must still be admitted. HasCtx is what makes that so.
func TestTurnEndUsageAtOrdinalZeroIsAccepted(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 0, 42_000, 0, 0))

	n, _ := r.Get("c1")
	if n.CtxTokens != 42_000 {
		t.Errorf("CtxTokens = %d, want 42000: the first turn_end, even at ordinal 0, must be accepted", n.CtxTokens)
	}
	if !n.HasCtx {
		t.Error("HasCtx = false, want true after the first turn_end")
	}
}

// An ordinal-less turn_end can never be deduped by ordinal, but taking it is
// safe: CtxTokens converges on the latest reading rather than accumulating,
// so there is nothing to double. CtxThrough stays untouched -- it has no
// ordinal to record.
func TestTurnEndUsageWithoutOrdinalIsAccepted(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsageNoOrdinal("c1", 7_000, 0, 0))

	n, _ := r.Get("c1")
	if n.CtxTokens != 7_000 {
		t.Errorf("CtxTokens = %d, want 7000: an ordinal-less turn_end must still be accepted", n.CtxTokens)
	}
	if n.CtxThrough != 0 {
		t.Errorf("CtxThrough = %d, want 0: an ordinal-less turn_end carries no watermark", n.CtxThrough)
	}

	// A later ordinalized turn_end still replaces it normally.
	r.Apply(turnEndWithUsage("c1", 3, 20_000, 0, 0))
	n, _ = r.Get("c1")
	if n.CtxTokens != 20_000 {
		t.Errorf("CtxTokens = %d, want 20000 after a later ordinalized turn_end", n.CtxTokens)
	}
	if n.CtxThrough != 3 {
		t.Errorf("CtxThrough = %d, want 3", n.CtxThrough)
	}
}

// A turn_end with no usage (Usage nil) must leave CtxTokens untouched -- it
// says nothing about the prompt size, so there is nothing to fold.
func TestTurnEndWithoutUsageLeavesCtxTokensAlone(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 1, 10_000, 0, 0))
	r.Apply(costTurnEnd("c1", 2, 0.10)) // no Usage set

	n, _ := r.Get("c1")
	if n.CtxTokens != 10_000 {
		t.Errorf("CtxTokens = %d, want 10000 unchanged by a usage-less turn_end", n.CtxTokens)
	}
	if n.CtxThrough != 1 {
		t.Errorf("CtxThrough = %d, want 1 unchanged by a usage-less turn_end", n.CtxThrough)
	}
}

// Seed copies ContextWindow into both a freshly-discovered node and one
// already in the rail -- the daemon is authoritative for it either way.
func TestSeedCopiesContextWindow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "fresh", Status: "idle", ContextWindow: 200_000},
	})
	n, ok := r.Get("c_1")
	if !ok {
		t.Fatal("c_1 not seeded")
	}
	if n.ContextWindow != 200_000 {
		t.Errorf("ContextWindow = %d, want 200000", n.ContextWindow)
	}

	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "fresh", Status: "idle", ContextWindow: 1_000_000},
	})
	n, _ = r.Get("c_1")
	if n.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow after re-seed = %d, want 1000000", n.ContextWindow)
	}
}
