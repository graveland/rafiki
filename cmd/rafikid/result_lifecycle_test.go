// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// prResultFixture: one controller whose durable child records land in a
// recordingChildStore (shared with shutdown_test.go), with a capturing event
// buffer and a manual clock — everything a result-lifecycle test needs to see
// both the store and the settle fragment.
func prResultFixture(t *testing.T) (*Controller, *recordingChildStore, *eventbuf.FakeClock, *capturedFlush) {
	t.Helper()
	clk := eventbuf.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })
	rec := &recordingChildStore{}
	c := &Controller{st: childstore.New(), cm: newChildManager(), evbuf: buf, children: rec}
	return c, rec, clk, cap
}

// prInsertChild inserts a coordinator and one parented child of the given
// kind under it, both idle.
func prInsertChild(t *testing.T, c *Controller, id, kind string) {
	t.Helper()
	c.st.Insert(&childstore.Session{
		ChildID: "c_coord", Name: "coord", Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	c.st.Insert(&childstore.Session{
		ChildID: id, Name: id, Kind: kind, Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_coord",
			childstore.LabelRoot:   "c_coord",
		},
	})
}

// TestResultClearedWhenLLMChildStartsTurn pins the per-turn rule for every
// LLM child kind: idle → running clears a stored result — an earlier turn's
// result must not ride a later settle fragment — and the persisted record is
// rewritten with the cleared value.
func TestResultClearedWhenLLMChildStartsTurn(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, kind := range []string{protocol.KindFundi, protocol.KindClaude} {
		c, rec, _, _ := prResultFixture(t)
		id := "c_" + kind
		prInsertChild(t, c, id, kind)
		_ = c.st.Update(id, func(s *childstore.Session) { s.Result = `{"stale":true}` })

		c.handleStatusChange(id, protocol.StatusRunning, protocol.StatusIdle)

		snap, ok := c.st.Get(id)
		ck.Require().True(ok, kind+": child vanished")
		ck.Eq("", snap.Result, kind+": the stored result must be cleared when a new turn starts")
		up, found := rec.lastUpsertFor(id)
		ck.Require().True(found, kind+": the clear must rewrite the persisted record")
		ck.Eq("", up.Result, kind+": the persisted record must carry the cleared result")
		ck.Eq(string(protocol.StatusRunning), up.Status, kind+": persisted record status")
	}
}

// TestResultSurvivesForScriptChildren pins the exemption: a script child's
// result is the work product of its whole run, so the same turn-start
// transition keeps it, in the store and in the persisted record.
func TestResultSurvivesForScriptChildren(t *testing.T) {
	ck := assert.NewAborting(t)
	c, rec, _, _ := prResultFixture(t)
	prInsertChild(t, c, "c_script", protocol.KindScript)
	_ = c.st.Update("c_script", func(s *childstore.Session) { s.Result = `{"answer":42}` })

	c.handleStatusChange("c_script", protocol.StatusRunning, protocol.StatusIdle)

	snap, ok := c.st.Get("c_script")
	ck.Require().True(ok, "child vanished")
	ck.Eq(`{"answer":42}`, snap.Result, "a script child's result must survive a turn start")
	up, found := rec.lastUpsertFor("c_script")
	ck.Require().True(found, "the transition must still persist the record")
	ck.Eq(`{"answer":42}`, up.Result, "the persisted record must carry the surviving result")
}

// TestResultNotClearedBetweenWorkingStatuses pins the guard on the
// transition, not on the destination: running → tool_running is working →
// working, so a result set mid-turn survives the status churn inside a turn.
func TestResultNotClearedBetweenWorkingStatuses(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := prResultFixture(t)
	prInsertChild(t, c, "c_w1", protocol.KindFundi)
	_, _ = c.st.SetStatus("c_w1", protocol.StatusRunning)
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.Result = `{"mid":true}` })

	c.handleStatusChange("c_w1", protocol.StatusToolRunning, protocol.StatusRunning)

	snap, ok := c.st.Get("c_w1")
	ck.Require().True(ok, "child vanished")
	ck.Eq(`{"mid":true}`, snap.Result, "a result set mid-turn must survive working→working status churn")
}

// TestResultSetDuringTurnRidesThatTurnsSettle pins the ordering the clear
// depends on: a result set DURING a turn rides THAT turn's settle. The
// turn-start clear removes only what an earlier turn stored; SetResult
// mid-turn re-stores, and the running → idle settle carries it.
func TestResultSetDuringTurnRidesThatTurnsSettle(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, clk, cap := prResultFixture(t)
	prInsertChild(t, c, "c_w1", protocol.KindFundi)
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.Result = `{"stale":1}` })

	c.handleStatusChange("c_w1", protocol.StatusRunning, protocol.StatusIdle)
	ck.Empty(cap.batches(), "a turn start must not settle anything")

	hub := c.connectScriptHub()
	ck.NoError(hub.SetResult(context.Background(), "c_w1", `"MID-TURN"`), "mid-turn SetResult")

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusRunning)
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.False(len(batches) != 1 || len(batches[0].fragments) != 1, "want 1 batch of 1 fragment, got %+v", batches)
	frag := batches[0].fragments[0]
	ck.StrContains(frag, "final result of c_w1: MID-TURN", "the mid-turn result must ride the settle; got %q", frag)
	ck.False(strings.Contains(frag, "stale"), "the earlier turn's result must be gone; got %q", frag)
}
