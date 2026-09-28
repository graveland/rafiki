// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"slices"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_live", "coordinator", "", "idle", 3),
		summary("c_dead", "old worker", "", "exited", 99),
	})
	c.Require().Eq(1, r.Len(), "Len")
	_, ok := r.Get("c_dead")
	c.False(ok, "c_dead must not be in the rail")
}

func TestSeedAcceptsEveryLiveStatus(t *testing.T) {
	c := assert.NewAborting(t)
	var sums []*rafikiv1.ChildSummary
	for i, st := range rail.LiveStatuses() {
		sums = append(sums, summary("c_"+st, st, "", st, int32(i)))
	}
	r := rail.New()
	r.Seed(sums)
	c.Eq(9, r.Len(), "Len = %d, want 9; LiveStatuses = %v", r.Len(), rail.LiveStatuses())
}

// A script child sits at "running" -- never "streaming"; its whole life is one
// run -- so the seed filter must admit it. LiveStatuses is what the cockpit
// sends as ListChildrenRequest.Statuses, and a filter without running made a
// script spawned before the cockpit attached invisible: no rail row, and every
// event from the unknown child then re-armed the reseed self-heal, which could
// not find it either. Pinned here at the rail level; the seed request itself
// is cockpit.go's seedCmd.
func TestSeedAcceptsRunningScriptChild(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(slices.Contains(rail.LiveStatuses(), "running"),
		"LiveStatuses must include running or the cockpit's seed filter hides script children")
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_script", "runner", "", "running", 4)})
	n, ok := r.Get("c_script")
	c.Require().True(ok, "a seeded running child must appear in the rail")
	c.Eq("running", n.Status, "Status")
}

func TestSeedPopulatesCwd(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Cwd: "/work/scout", Labels: map[string]string{}},
	})
	n, ok := r.Get("c_1")
	c.Require().True(ok, "c_1 not seeded")
	c.Eq("/work/scout", n.Cwd, "Cwd")

	// A re-seed refreshes Cwd the same way it refreshes Name and SessionID --
	// it is daemon-authoritative, not client reading history.
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "scout", Status: "idle", Cwd: "/work/scout-renamed", Labels: map[string]string{}},
	})
	n, _ = r.Get("c_1")
	c.Eq("/work/scout-renamed", n.Cwd, "Cwd after re-seed")
}

func TestSeedPopulatesMaxCost(t *testing.T) {
	c := assert.NewCollecting(t)
	cap1 := 5.0
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &cap1},
		{ChildId: "c_2", Name: "uncapped", Status: "idle", Labels: map[string]string{}},
	})
	n, ok := r.Get("c_1")
	c.Require().True(ok, "c_1 not seeded")
	c.Eq(5.0, n.MaxCost, "MaxCost")
	n2, ok := r.Get("c_2")
	c.Require().True(ok, "c_2 not seeded")
	c.Eq(0, n2.MaxCost, "MaxCost")

	// A re-seed refreshes MaxCost the same way it refreshes Cwd/SessionID --
	// daemon-authoritative, not client reading history.
	cap2 := 10.0
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &cap2},
	})
	n, _ = r.Get("c_1")
	c.Eq(10.0, n.MaxCost, "MaxCost after re-seed")
}

func TestSeedCarriesKind(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_a", Name: "worker", Kind: "claude", Status: "idle"},
		{ChildId: "c_b", Name: "native", Kind: "fundi", Status: "idle"},
	})
	for _, tc := range []struct{ id, want string }{{"c_a", "claude"}, {"c_b", "fundi"}} {
		n, ok := r.Get(tc.id)
		c.Require().True(ok, "no node for %q", tc.id)
		c.Eq(tc.want, n.Kind, "Node(%q).Kind = %q, want", tc.id, n.Kind)
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
	c := assert.NewAborting(t)
	r := rail.New()
	capVal := 10.0
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "capped", Status: "idle", Labels: map[string]string{}, MaxCost: &capVal},
	})

	// Raising MaxCost
	r.SetMaxCost("c_1", 20.0)
	n, ok := r.Get("c_1")
	c.False(!ok || n.MaxCost != 20.0, "MaxCost = %v, want 20.0", n.MaxCost)

	// Lowering MaxCost (must actually lower, unlike SetCost which takes max)
	r.SetMaxCost("c_1", 5.0)
	n, _ = r.Get("c_1")
	c.Eq(5.0, n.MaxCost, "MaxCost")

	// Clearing MaxCost (setting to 0)
	r.SetMaxCost("c_1", 0)
	n, _ = r.Get("c_1")
	c.Eq(0, n.MaxCost, "MaxCost")

	// Unknown child ID is a silent no-op
	r.SetMaxCost("c_unknown", 100.0)
}

func TestWorkingMatchesTheMidTurnStatuses(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, st := range []string{"streaming", "tool_running", "compacting"} {
		c.True(rail.Working(st), "Working(%q) = false, want true", st)
	}
	for _, st := range []string{"spawning", "idle", "blocked_ui", "shutting_down", "exited", "running", ""} {
		c.False(rail.Working(st), "Working(%q) = true, want false", st)
	}
}

func TestChildSpawnedAddsARow(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_root", "coordinator", "", "idle", 0)})
	r.Apply(spawned("c_kid", "c_root", "scout", 0))

	c.Require().Eq(2, r.Len(), "Len")
	n, ok := r.Get("c_kid")
	c.Require().True(ok, "c_kid missing")
	c.False(n.ParentID != "c_root" || n.Name != "scout", "node = %+v, want parent c_root name scout", n)
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
	assert.NewCollecting(t).False(!n.Exited || n.ExitCode == nil || *n.ExitCode != 0, "node = %+v, want exited with code 0", n)
}

func TestDisplayOrderIsParentThenChildren(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.Len(got, len(want), "order = %v, want %v", got, want)
	for i := range want {
		c.Eq(want[i], got[i], "order = %v, want %v", got, want)
	}
	if d, _ := r.Get("c_a"); d.Depth != 1 {
		t.Errorf("c_a depth = %d, want 1", d.Depth)
	}
}

func TestEventForAnUnknownChildIsIgnored(t *testing.T) {
	r := rail.New()
	r.Apply(exited("c_ghost", 1, 0))
	assert.NewAborting(t).Eq(0, r.Len(), "Len")
}

// A reconnect re-seeds to discover children spawned during the disconnect,
// whose child_spawned is in the past. That must not wipe reading history.
func TestReSeedPreservesWatermarkAndBadge(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Eq(1, n.Attention, "attention")
	c.Eq(0, n.Seen, "seen")
	c.Eq(2, r.Len(), "Len")
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
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(assistantMessageWithCost("c1", 1, 0.50))
	r.Apply(costTurnEnd("c1", 2, 0.60)) // the turn's own final total, slightly
	// different from the last live reading
	// (pricing can differ per-component
	// between a partial and final usage)

	n, _ := r.Get("c1")
	c.Eq(0.60, n.Cost, "Cost")
	c.Eq(0, n.CostLive, "CostLive")
	c.Eq(0.60, n.TotalCost(), "TotalCost")

	// A second turn's first reply starts its own running total.
	r.Apply(assistantMessageWithCost("c1", 3, 0.05))
	n, _ = r.Get("c1")
	c.Eq(0.65, n.TotalCost(), "TotalCost after 2nd turn's 1st reply")
}

func TestSubtreeCostIncludesLiveCost(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(spawned("c2", "c1", "worker", 0))
	r.Apply(costTurnEnd("c1", 0, 1.0))
	r.Apply(assistantMessageWithCost("c2", 1, 0.30))

	assert.NewCollecting(t).Eq(1.30, r.SubtreeCost("c1"), "SubtreeCost(c1)")
}

// TurnEnd carries the cost of ONE turn (Emitter.AgentEnd resets its usage), so
// costs are summed.
func TestRailSumsTurnCost(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 0, 0.25))
	r.Apply(costTurnEnd("c1", 1, 0.75))

	n, ok := r.Get("c1")
	c.Require().True(ok, "c1 missing")
	c.Eq(1.0, n.Cost, "Cost")
}

// The rail and focus subscriptions overlap on the durable tier, so the same
// turn_end arrives twice. Summing without a watermark doubles the bill.
func TestRailDoesNotDoubleCountADuplicateTurnEnd(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 7, 0.50))
	r.Apply(costTurnEnd("c1", 7, 0.50))

	n, _ := r.Get("c1")
	assert.NewCollecting(t).Eq(0.50, n.Cost, "Cost")
}

// A focused agent's headline number includes what its subagents spent.
func TestSubtreeCostSumsDescendants(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(spawned("c2", "c1", "worker", 0))
	r.Apply(spawned("c3", "c2", "deep", 0))
	r.Apply(costTurnEnd("c1", 0, 1.0))
	r.Apply(costTurnEnd("c2", 0, 2.0))
	r.Apply(costTurnEnd("c3", 0, 4.0))

	c.Eq(7.0, r.SubtreeCost("c1"), "SubtreeCost(c1)")
	c.Eq(6.0, r.SubtreeCost("c2"), "SubtreeCost(c2)")
	c.Eq(4.0, r.SubtreeCost("c3"), "SubtreeCost(c3)")
}

// A seed from ListChildren must not be undone by a later stream event, and
// must not be added to twice.
func TestSetCostSeedsWithoutDoubleCounting(t *testing.T) {
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.SetCost("c1", 5.0)
	r.SetCost("c1", 5.0)
	n, _ := r.Get("c1")
	assert.NewCollecting(t).Eq(5.0, n.Cost, "Cost")
}

// A re-seed carrying a rollup computed before the newest conversation_turn
// rows were visible must not erase cost this rail already counted: those
// ordinals are below CostThrough and Apply will never re-add them.
func TestSetCostNeverLowersAnAccumulatedTotal(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(costTurnEnd("c1", 0, 3.0))
	r.SetCost("c1", 1.0) // a lagging server rollup

	n, _ := r.Get("c1")
	c.Eq(3.0, n.Cost, "Cost")

	// The seed IS authoritative for turns that predate this client, which the
	// stream never replays -- those only ever raise the number.
	r.SetCost("c1", 9.0)
	n, _ = r.Get("c1")
	c.Eq(9.0, n.Cost, "Cost")
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
	assert.NewCollecting(t).Eq("idle", n.Status, "Status")
}

// A ChildExited must not be undone by a stale AgentStatus that outraces it
// through the other feed.
func TestApplyIgnoresAStaleStatusArrivingAfterExit(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(exited("c1", 0, 10))
	r.Apply(statusEvt("c1", 3, "tool_running"))

	n, _ := r.Get("c1")
	c.True(n.Exited, "Exited = false, want true: a stale status must not un-exit the row")
	c.Eq("exited", n.Status, "Status")
}

func TestRemoveDropsTheNodeAndClearsFocus(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Eq("", r.Focus(), "Focus()")
	_, ok := r.Get("c_2")
	c.True(ok, "Remove took an unrelated node with it")
}

func TestRemoveIsIdempotent(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{{ChildId: "c_1", Status: "idle"}})
	r.Remove("c_1")
	r.Remove("c_1") // must not panic
	assert.NewCollecting(t).Eq(0, r.Len(), "Len()")
}

// turn_end usage sets CtxTokens to the prompt size the NEXT call will carry:
// input + cache-read + cache-write, and records the watermark it landed at.
func TestTurnEndUsageSetsCtxTokens(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 1, 10_000, 50_000, 2_000))

	n, _ := r.Get("c1")
	c.Eq(62_000, n.CtxTokens, "CtxTokens")
	c.Eq(1, n.CtxThrough, "CtxThrough")
}

// The rail and focus subscriptions overlap on the durable tier, so an older
// turn_end can arrive after a newer one already landed. The watermark must
// stop it from regressing the displayed figure, same as CostThrough.
func TestTurnEndUsageReplayDoesNotRegressCtxTokens(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 5, 100_000, 0, 0))
	r.Apply(turnEndWithUsage("c1", 2, 1_000, 0, 0)) // stale replay, lower ordinal

	n, _ := r.Get("c1")
	c.Eq(100_000, n.CtxTokens, "CtxTokens")
	c.Eq(5, n.CtxThrough, "CtxThrough")
}

// Unlike Cost, CtxTokens is a reading, not a running total -- ordinal 0 is
// both a legal ordinal and CtxThrough's zero value, so the child's very
// first turn_end must still be admitted. HasCtx is what makes that so.
func TestTurnEndUsageAtOrdinalZeroIsAccepted(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 0, 42_000, 0, 0))

	n, _ := r.Get("c1")
	c.Eq(42_000, n.CtxTokens, "CtxTokens")
	c.True(n.HasCtx, "HasCtx = false, want true after the first turn_end")
}

// An ordinal-less turn_end can never be deduped by ordinal, but taking it is
// safe: CtxTokens converges on the latest reading rather than accumulating,
// so there is nothing to double. CtxThrough stays untouched -- it has no
// ordinal to record.
func TestTurnEndUsageWithoutOrdinalIsAccepted(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsageNoOrdinal("c1", 7_000, 0, 0))

	n, _ := r.Get("c1")
	c.Eq(7_000, n.CtxTokens, "CtxTokens")
	c.Eq(0, n.CtxThrough, "CtxThrough")

	// A later ordinalized turn_end still replaces it normally.
	r.Apply(turnEndWithUsage("c1", 3, 20_000, 0, 0))
	n, _ = r.Get("c1")
	c.Eq(20_000, n.CtxTokens, "CtxTokens")
	c.Eq(3, n.CtxThrough, "CtxThrough")
}

// A turn_end with no usage (Usage nil) must leave CtxTokens untouched -- it
// says nothing about the prompt size, so there is nothing to fold.
func TestTurnEndWithoutUsageLeavesCtxTokensAlone(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Apply(spawned("c1", "", "root", 0))
	r.Apply(turnEndWithUsage("c1", 1, 10_000, 0, 0))
	r.Apply(costTurnEnd("c1", 2, 0.10)) // no Usage set

	n, _ := r.Get("c1")
	c.Eq(10_000, n.CtxTokens, "CtxTokens")
	c.Eq(1, n.CtxThrough, "CtxThrough")
}

// Seed copies ContextWindow into both a freshly-discovered node and one
// already in the rail -- the daemon is authoritative for it either way.
func TestSeedCopiesContextWindow(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "fresh", Status: "idle", ContextWindow: 200_000},
	})
	n, ok := r.Get("c_1")
	c.Require().True(ok, "c_1 not seeded")
	c.Eq(200_000, n.ContextWindow, "ContextWindow")

	r.Seed([]*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "fresh", Status: "idle", ContextWindow: 1_000_000},
	})
	n, _ = r.Get("c_1")
	c.Eq(1_000_000, n.ContextWindow, "ContextWindow after re-seed")
}
