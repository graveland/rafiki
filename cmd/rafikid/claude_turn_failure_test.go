// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

func makeClaude(t *testing.T, c *Controller, id string, quiet bool) {
	t.Helper()
	assert.NewAborting(t).NoError(c.st.Update(id, func(s *childstore.Session) {
		s.Kind = protocol.KindClaude
		if quiet {
			s.NotifyPolicy = protocol.NotifyResult
		}
	}))
}

func idleClaude(c *Controller, id string) {
	c.handleStatusChange(id, protocol.StatusIdle, protocol.StatusStreaming)
}

func fragmentText(cap *capturedFlush) string {
	var all []string
	for _, b := range coordBatches(cap) {
		all = append(all, b.fragments...)
	}
	return strings.Join(all, "\n")
}

func TestClaudeTurnFailureBeforeIdleWakesAQuietSpawner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	makeClaude(t, c, "c_w1", true)
	makeClaude(t, c, "c_w2", true)

	c.noteClaudeTurnFailure("c_w1", "upstream status 529: Overloaded")
	idleClaude(c, "c_w1")
	idleClaude(c, "c_w2")
	clk.Advance(6 * time.Second)

	got := fragmentText(cap)
	ck.StrContains(got, "turn error", "the failed turn's settle reaches the spawner")
	ck.StrContains(got, "529", "the settle carries the proxy's reason")
	ck.Eq(1, strings.Count(got, "settled"), "only the failed child's settle is sent; the quiet child with no failure stays silent: %s", got)
}

func TestClaudeTurnFailureAfterIdleWakesAQuietSpawnerOnce(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	makeClaude(t, c, "c_w1", true)

	idleClaude(c, "c_w1")
	clk.Advance(6 * time.Second)
	ck.Empty(coordBatches(cap), "the plain idle transition of a quiet child is dropped")

	c.noteClaudeTurnFailure("c_w1", "upstream status 529: Overloaded")
	clk.Advance(6 * time.Second)

	ck.StrContains(fragmentText(cap), "turn error", "the report that lost the race to the result frame still wakes the spawner")
	ck.False(c.turnOutcomes.uncleanPending("c_w1"), "no outcome is left behind for a later, unrelated idle transition")
}

func TestClaudeTurnFailureFollowedBySuccessWakesNobody(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	makeClaude(t, c, "c_w1", true)

	c.noteClaudeTurnFailure("c_w1", "upstream status 529: Overloaded")
	c.clearClaudeTurnFailure("c_w1")
	idleClaude(c, "c_w1")
	clk.Advance(6 * time.Second)

	ck.Empty(coordBatches(cap), "a retry that succeeded leaves a quiet spawner asleep")
	snap, _ := c.st.Get("c_w1")
	ck.Eq("", snap.LastTurnError, "the recovered failure is not left on the row")
	ck.False(c.turnOutcomes.uncleanPending("c_w1"), "nor in the outcome store")
}

func TestClaudeTurnFailureOnADefaultChildDoesNotDuplicateItsSettle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	makeClaude(t, c, "c_w1", false)

	idleClaude(c, "c_w1")
	clk.Advance(6 * time.Second)
	before := fragmentText(cap)
	ck.StrContains(before, "settled", "a default child's turn end always wakes")

	c.noteClaudeTurnFailure("c_w1", "upstream status 529: Overloaded")
	clk.Advance(6 * time.Second)

	ck.Eq(before, fragmentText(cap), "a late failure adds no second settle for a default child")
	snap, _ := c.st.Get("c_w1")
	ck.StrContains(snap.LastTurnError, "529", "the failure still reaches the row, so the display state reads failed")
}

func TestClaudeTurnFailureIsIgnoredForOtherKinds(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _, _ := settleFixture(t)
	ck.NoError(c.st.Update("c_w1", func(s *childstore.Session) { s.Kind = protocol.KindFundi }))

	c.noteClaudeTurnFailure("c_w1", "upstream status 529")

	snap, _ := c.st.Get("c_w1")
	ck.Eq("", snap.LastTurnError, "a fundi child records its own turn outcome, never the proxy's")
}

func TestCaptureReportedRecordsAndClearsAClaudeTurnFailure(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	failed := func() string {
		snap, _ := h.c.st.Get("c_1")
		return snap.LastTurnError
	}

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "cv", ReleaseOnly: true, TurnErr: "titler failed"})
	ck.Eq("", failed(), "a helper request's failure is not the agent's turn")

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "cv", TurnErr: "upstream status 529: Overloaded"})
	ck.StrContains(failed(), "529", "a failed main-thread request is recorded on the child")

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "cv", ReleaseOnly: true, ResponseMessageID: "m_helper"})
	ck.StrContains(failed(), "529", "a helper's success does not clear the agent's failure")

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "cv", Err: "append failed"})
	ck.StrContains(failed(), "529", "a capture that was not persisted says nothing about the turn")

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "cv", ResponseMessageID: "m_ok"})
	ck.Eq("", failed(), "a later successful main-thread request clears it")
}
