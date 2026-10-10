package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// fakeClearMarker records the /clear marks it is asked to make, and can fail on
// demand so the mark-before-arm ordering is testable.
type fakeClearMarker struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeClearMarker) MarkClearPending(_ context.Context, externalRef string) error {
	f.mu.Lock()
	f.calls = append(f.calls, externalRef)
	f.mu.Unlock()
	return f.err
}

func (f *fakeClearMarker) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// slashFixture builds a controller whose inbox records what was accepted (so a
// "queued" assertion sees the row even when delivery consumes it) and whose
// clear marker is a recording fake.
func slashFixture(t *testing.T) (*Controller, *acceptRecorder, *fakeClearMarker) {
	t.Helper()
	ctrl := newTestController(t)
	rec := &acceptRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)
	marker := &fakeClearMarker{}
	ctrl.clearMarker = marker
	return ctrl, rec, marker
}

func spawnFundiChild(t *testing.T, ctrl *Controller) string {
	t.Helper()
	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindFundi,
		Model:     "anthropic/sonnet-latest",
		Cwd:       t.TempDir(),
		NoSession: true,
	}, users.Identity{})
	assert.NewAborting(t).Require().NoError(err, "spawn fundi")
	return res.ChildID
}

// A /clear on a claude child marks the conversation, arms the session-id guard
// and then queues the text like any other prompt.
func TestSlashClearOnClaudeArmsMarksAndQueues(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, marker := slashFixture(t)
	id := spawnTestChild(t, ctrl, nil)

	got, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: id, Mode: inbox.ModePrompt, Text: "/clear"})
	ck.Require().NoError(err, "Accept(/clear)")
	ck.NotEq("", got, "a /clear queues a prompt, so its row id must come back")

	ck.EqDeep([]string{id}, marker.called(), "the clear marker must be called once with the child id")
	if _, armed := ctrl.clearExpected.Load(id); !armed {
		t.Errorf("a /clear must arm the session-id guard for %s", id)
	}
	rows := rec.accepted()
	ck.False(len(rows) != 1 || rows[0].Mode != inbox.ModePrompt || rows[0].Text != "/clear",
		"queued rows = %+v; want one prompt carrying /clear", rows)
}

// Mark first, arm second: a failed mark must leave nothing armed and queue no
// row, or the next unrelated session id change would be adopted as a clear.
func TestSlashClearMarkErrorArmsNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, marker := slashFixture(t)
	marker.err = errors.New("capture store down")
	id := spawnTestChild(t, ctrl, nil)

	_, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: id, Mode: inbox.ModePrompt, Text: "/clear"})
	ck.Require().Error(err, "a failed mark must surface")
	if _, armed := ctrl.clearExpected.Load(id); armed {
		t.Errorf("a failed mark armed the session-id guard for %s; arm must follow mark", id)
	}
	ck.Empty(rec.accepted(), "a failed mark must queue nothing; store holds")
}

// A fundi engine clears its own history when the prompt reaches it, so the
// daemon queues /clear untouched: no capture mark, no session-id arm.
func TestSlashClearAndCompactOnFundiQueueUntouched(t *testing.T) {
	for _, text := range []string{"/clear", "/compact", "/compact keep the schema"} {
		t.Run(text, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ctrl, rec, marker := slashFixture(t)
			ctrl.st.Insert(&childstore.Session{ChildID: "c_fundi", Kind: protocol.KindFundi, Status: protocol.StatusIdle})

			_, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: "c_fundi", Mode: inbox.ModePrompt, Text: text})
			ck.Require().NoError(err, "Accept(%s) for a fundi child", text)
			rows := rec.accepted()
			ck.Require().Eq(1, len(rows), "queued rows = %+v; want one prompt", rows)
			ck.Eq(text, rows[0].Text, "the prompt must reach the engine verbatim")
			ck.Empty(marker.called(), "a fundi %s must not mark capture", text)
			if _, armed := ctrl.clearExpected.Load("c_fundi"); armed {
				t.Errorf("a fundi %s must not arm the session-id guard", text)
			}
		})
	}
}

func TestSlashClearOnScriptRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, marker := slashFixture(t)
	ctrl.st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: protocol.StatusIdle})

	_, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: "c_script", Mode: inbox.ModePrompt, Text: "/clear"})
	var ce *connectapi.ControllerError
	ck.False(!errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs,
		"Accept(/clear) for a script child = %v; want a coded invalid_args error", err)
	ck.Empty(rec.accepted(), "a refused /clear must queue nothing")
	ck.Empty(marker.called(), "a refused /clear must not mark")
	if _, armed := ctrl.clearExpected.Load("c_script"); armed {
		t.Errorf("a refused /clear must not arm the session-id guard")
	}
}

// /exit acts synchronously: it kills the child and returns an empty id, and
// nothing is queued.
func TestSlashExitKillsAndQueuesNothing(t *testing.T) {
	for _, kind := range []string{protocol.KindClaude, protocol.KindFundi} {
		t.Run(kind, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ctrl, rec, _ := slashFixture(t)

			var id string
			if kind == protocol.KindClaude {
				id = spawnTestChild(t, ctrl, nil)
			} else {
				id = spawnFundiChild(t, ctrl)
			}

			got, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: id, Mode: inbox.ModePrompt, Text: "/exit"})
			ck.Require().NoError(err, "Accept(/exit)")
			ck.Eq("", got, "/exit acts synchronously and has no row id to quote back")
			ck.Empty(rec.accepted(), "a /exit must queue nothing; store holds")
			waitForExited(t, ctrl.st, id, 10*time.Second)
		})
	}
}

// Only a registered command name is interpreted, and only as a prompt: an
// unknown slash text is an ordinary prompt, and a steer never reaches the
// command layer at all.
func TestSlashUnknownAndSteerAreOrdinaryPrompts(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, marker := slashFixture(t)
	id := spawnTestChild(t, ctrl, nil)

	_, err := ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: id, Mode: inbox.ModePrompt, Text: "/unknown"})
	ck.Require().NoError(err, "Accept(/unknown)")
	rows := rec.accepted()
	ck.False(len(rows) != 1 || rows[0].Mode != inbox.ModePrompt || rows[0].Text != "/unknown",
		"an unknown slash text must be queued as an ordinary prompt; got %+v", rows)

	_, err = ctrl.connectInbox().Accept(t.Context(), inbox.Inbound{ChildID: id, Mode: inbox.ModeSteer, Text: "/clear"})
	ck.Require().NoError(err, "Accept(steer /clear)")
	rows = rec.accepted()
	ck.False(len(rows) != 2 || rows[1].Mode != inbox.ModeSteer || rows[1].Text != "/clear",
		"a /clear sent as a steer must be queued as steer text; got %+v", rows)
	ck.Empty(marker.called(), "steer text must never be interpreted as a command")
	if _, armed := ctrl.clearExpected.Load(id); armed {
		t.Errorf("a /clear sent as a steer must not arm the session-id guard")
	}
}

// An armed claude child adopts the next session id it reports: the row moves to
// the new id, nothing is labelled an error and the child keeps running.
func TestClaudeClearAdoptsTheNextSessionID(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, next = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb"
	id := resumedClaudeChild(t, ctrl, orig)

	ctrl.clearExpected.Store(id, struct{}{})
	emitClaudeInit(t, ctrl, id, next)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && mustSnapshot(t, ctrl, id).SessionID != next {
		time.Sleep(20 * time.Millisecond)
	}
	snap := mustSnapshot(t, ctrl, id)
	ck.Eq(next, snap.SessionID, "an armed /clear must adopt the new session id")
	ck.False(snap.Status == protocol.StatusExited, "adopting the /clear id must not end the child")
	ck.Eq("", snap.Labels["rafiki/session-error"], "the /clear adoption must not label the row an error")
	if _, armed := ctrl.clearExpected.Load(id); armed {
		t.Errorf("the /clear arm must be consumed by the adoption")
	}
}

// The guard runs from two call sites with different views of the held id. Once
// one adopts the new id, the other's stale held must not be read as a change.
func TestClaudeClearSecondCallSiteDoesNotKill(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, next = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb"
	id := resumedClaudeChild(t, ctrl, orig)

	ctrl.clearExpected.Store(id, struct{}{})
	ck.False(ctrl.refuseClaudeSessionIDChange(id, orig, next), "the first report must adopt, not refuse")
	ck.False(ctrl.refuseClaudeSessionIDChange(id, orig, next), "the second call site, holding a stale id, must not end the child")

	time.Sleep(200 * time.Millisecond)
	snap := mustSnapshot(t, ctrl, id)
	ck.False(snap.Status == protocol.StatusExited, "the stale-held report ended the child")
	ck.Eq(next, snap.SessionID, "the adopted id was overwritten")
}

// The arm is one-shot: after a /clear adopts id B, a later report of id C is an
// ordinary change and ends the child, with the row keeping B.
func TestClaudeClearArmIsOneShot(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, adopted, third = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb", "11111111-2222-3333-4444-555555555555"
	id := resumedClaudeChild(t, ctrl, orig)

	ctrl.clearExpected.Store(id, struct{}{})
	ck.False(ctrl.refuseClaudeSessionIDChange(id, orig, adopted), "the first report must adopt")
	ck.True(ctrl.refuseClaudeSessionIDChange(id, adopted, third), "a later, unarmed change must be refused")

	waitForExited(t, ctrl.st, id, 10*time.Second)
	snap := mustSnapshot(t, ctrl, id)
	ck.Eq(adopted, snap.SessionID, "the row must keep the adopted id, not the refused one")
	ck.True(strings.Contains(snap.Labels["rafiki/session-error"], third),
		"the row must say why the child was ended: %q", snap.Labels["rafiki/session-error"])
}

// A relaunch must not inherit a /clear arm: activateLiveChild clears it.
func TestActivateLiveChildClearsTheArm(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t), NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")
	id := res.ChildID

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, id, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, id, 5*time.Second)

	ctrl.clearExpected.Store(id, struct{}{})
	_, err = ctrl.Resume(t.Context(), id, "")
	ck.Require().NoError(err, "resume")

	if _, armed := ctrl.clearExpected.Load(id); armed {
		t.Errorf("a relaunch inherited a /clear arm; activateLiveChild must clear it")
	}
}

// A slash command sent through the framed Controller.Send path is intercepted
// too, not only through the Connect accepter. Deleting the handleSlashCommand
// call from Controller.Send makes this queue a row and leave the child running.
func TestSendSlashExitViaFramedPathKillsAndQueuesNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, _ := slashFixture(t)
	id := spawnTestChild(t, ctrl, nil)

	ck.Require().NoError(ctrl.Send(id, json.RawMessage(`{"type":"prompt","message":"/exit"}`)), "Send(/exit)")
	ck.Empty(rec.accepted(), "a /exit via the framed path must queue nothing")
	waitForExited(t, ctrl.st, id, 10*time.Second)
}

// The OnMeta hook and monitorChild both report the same system/init. Their
// concurrent calls to the guard must not let the stale report kill a child
// whose /clear was just adopted: exactly one adoption, no kill, the row ends
// on the new id and the arm is consumed. Fails without sessionIDMu serializing
// the store-holds check, the adoption and the store Update.
func TestClaudeClearConcurrentReportsAdoptOnceAndDoNotKill(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, next = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb"
	ctrl.st.Insert(&childstore.Session{ChildID: "c_clear", Kind: protocol.KindClaude, Status: protocol.StatusIdle, SessionID: orig})

	for i := 0; i < 200; i++ {
		ck.Require().NoError(ctrl.st.Update("c_clear", func(s *childstore.Session) { s.SessionID = orig }), "reset row")
		ctrl.sessionIDEnded.Delete("c_clear")
		ctrl.clearExpected.Store("c_clear", struct{}{})

		var wg sync.WaitGroup
		var a, b bool
		wg.Add(2)
		go func() { defer wg.Done(); a = ctrl.refuseClaudeSessionIDChange("c_clear", orig, next) }()
		go func() { defer wg.Done(); b = ctrl.refuseClaudeSessionIDChange("c_clear", orig, next) }()
		wg.Wait()

		ck.False(a || b, "iteration %d: a concurrent duplicate report was treated as a change", i)
		snap, ok := ctrl.st.Get("c_clear")
		ck.Require().True(ok, "iteration %d: row vanished", i)
		ck.Eq(next, snap.SessionID, "iteration %d: the armed /clear must adopt the new id", i)
		ck.False(snap.Status == protocol.StatusExited, "iteration %d: a concurrent duplicate report killed the child", i)
		if _, armed := ctrl.clearExpected.Load("c_clear"); armed {
			t.Fatalf("iteration %d: the arm was not consumed", i)
		}
	}
}

// The store-holds early return also holds with no arm present: a report the
// store already carries is not a change and must not end the child.
func TestClaudeClearStoreAlreadyHoldsReportedIsNotAChange(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	const orig, next = "7bd6b824-5b45-455f-95f7-36f7b34fc7aa", "ec3af0b5-dc54-49e9-8b61-addfeb680cbb"
	ctrl.st.Insert(&childstore.Session{ChildID: "c_holds", Kind: protocol.KindClaude, Status: protocol.StatusIdle, SessionID: next})

	ck.False(ctrl.refuseClaudeSessionIDChange("c_holds", orig, next), "a report the store already holds must not be a change")

	time.Sleep(100 * time.Millisecond)
	snap, ok := ctrl.st.Get("c_holds")
	ck.Require().True(ok, "row vanished")
	ck.Eq(next, snap.SessionID, "the store-held id was overwritten")
	ck.False(snap.Status == protocol.StatusExited, "the store-held report ended the child")
}
