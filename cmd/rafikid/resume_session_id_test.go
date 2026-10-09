package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// A claude child is silent until prompted, so a freshly resumed process has
// not reported a session id yet. The resumed row must still carry the id it was
// resumed from: a second relaunch before the first turn would otherwise find
// nothing to --resume and start a brand-new conversation.
func TestResumedClaudeChildKeepsItsSessionIDBeforeTheFirstTurn(t *testing.T) {
	ck := assert.NewAborting(t)

	ctrl := newTestController(t)
	ctx := t.Context()

	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")

	const sid = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa"
	ck.Require().NoError(ctrl.st.Update(res.ChildID, func(s *childstore.Session) { s.SessionID = sid }), "seed session id")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)

	_, err = ctrl.Resume(ctx, res.ChildID, "")
	ck.Require().NoError(err, "resume")

	ck.Eq(sid, mustSnapshot(t, ctrl, res.ChildID).SessionID,
		"the resumed row dropped the session id, so the next relaunch has no --resume token")
}

// resumedClaudeChild spawns a claude child, gives it session id sid, kills it
// and resumes it, so monitorChild starts with sid as the id it expects.
func resumedClaudeChild(t *testing.T, ctrl *Controller, sid string) string {
	t.Helper()
	ck := assert.NewAborting(t)

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")
	ck.Require().NoError(ctrl.st.Update(res.ChildID, func(s *childstore.Session) { s.SessionID = sid }), "seed session id")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)

	_, err = ctrl.Resume(t.Context(), res.ChildID, "")
	ck.Require().NoError(err, "resume")
	return res.ChildID
}

// emitClaudeInit makes the live child report a claude system/init carrying
// sessionID, then the first frame of a turn. system/init produces no bus frame
// of its own and monitorChild syncs metadata on bus frames, so the assistant
// frame the fake follows it with is what wakes the sync.
func emitClaudeInit(t *testing.T, ctrl *Controller, childID, sessionID string) {
	t.Helper()
	ch, ok := ctrl.cm.Get(childID)
	assert.NewAborting(t).Require().True(ok, "child %s is not live", childID)
	frame := `{"type":"prompt","message":"__claude_init:` + sessionID + `"}`
	assert.NewAborting(t).Require().NoError(ch.Send([]byte(frame)), "send prompt")
}

// A claude child whose reported session id differs from the one it was resumed
// from has started a different conversation under the same child. That must end
// the child loudly, with the original id kept on the row so a manual resume
// returns to the right transcript — not overwrite the id and carry on.
func TestClaudeSessionIDChangeKillsTheChildAndKeepsTheOriginalID(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, other = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb"
	id := resumedClaudeChild(t, ctrl, orig)

	emitClaudeInit(t, ctrl, id, other)
	waitForExited(t, ctrl.st, id, 10*time.Second)

	snap := mustSnapshot(t, ctrl, id)
	ck.Eq(orig, snap.SessionID, "the changed id overwrote the original, so the original conversation can no longer be resumed")
	ck.True(strings.Contains(snap.Labels["rafiki/session-error"], other), "the row does not say why the child was ended: %q", snap.Labels["rafiki/session-error"])
}

// Re-reporting the id a child already has is the normal case on every turn and
// must not be mistaken for a change.
func TestClaudeSameSessionIDReportedAgainIsNotAChange(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const sid = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa"
	id := resumedClaudeChild(t, ctrl, sid)

	emitClaudeInit(t, ctrl, id, sid)
	time.Sleep(500 * time.Millisecond)

	snap := mustSnapshot(t, ctrl, id)
	ck.True(snap.Status != protocol.StatusExited, "a repeated session id ended the child")
	ck.Eq(sid, snap.SessionID, "session id changed")
}

// A child that has never reported an id (a fresh spawn, before its first turn)
// has nothing to protect: the first id it reports is adopted.
func TestClaudeFirstReportedSessionIDIsAdopted(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t), NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")

	const sid = "11111111-2222-3333-4444-555555555555"
	emitClaudeInit(t, ctrl, res.ChildID, sid)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && mustSnapshot(t, ctrl, res.ChildID).SessionID != sid {
		time.Sleep(20 * time.Millisecond)
	}
	snap := mustSnapshot(t, ctrl, res.ChildID)
	ck.Eq(sid, snap.SessionID, "the first reported id was not adopted")
	ck.True(snap.Status != protocol.StatusExited, "adopting the first id ended the child")
}

// A claude child with captured history but no recorded session id cannot be
// resumed: --resume has nothing to name, so claude would start a fresh session
// whose first request lands in the old child's conversation. A child that never
// reached a turn has no history and resumes fresh harmlessly.
func TestClaudeResumeNeedsASessionIDWhenHistoryExists(t *testing.T) {
	ck := assert.NewCollecting(t)

	claude := childstore.Snapshot{Kind: protocol.KindClaude}
	withID := childstore.Snapshot{Kind: protocol.KindClaude, SessionID: "sess-1"}
	fundi := childstore.Snapshot{Kind: protocol.KindFundi}

	ck.Error(checkClaudeResumeToken(claude, true), "history but no session id must refuse")
	ck.NoError(checkClaudeResumeToken(claude, false), "no history: a fresh session loses nothing")
	ck.NoError(checkClaudeResumeToken(withID, true), "an id to resume is the normal case")
	ck.NoError(checkClaudeResumeToken(fundi, true), "only claude resumes by session id")
}
