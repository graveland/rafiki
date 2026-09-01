// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestBuildInjectionFrameShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch inbox.Batch
		id    string
		want  string
	}{
		{
			name:  "direct prompt",
			batch: inbox.Batch{ChildID: "c_1", Mode: inbox.ModePrompt, Frags: []string{"hello"}},
			id:    "F1",
			want:  `{"type":"prompt","id":"F1","message":"hello"}`,
		},
		{
			name:  "direct steer",
			batch: inbox.Batch{ChildID: "c_1", Mode: inbox.ModeSteer, Frags: []string{"stop"}},
			id:    "F1",
			want:  `{"type":"steer","id":"F1","message":"stop"}`,
		},
		{
			name:  "abort carries no id: nothing acks it",
			batch: inbox.Batch{ChildID: "c_1", Mode: inbox.ModeAbort},
			id:    "F1",
			want:  `{"type":"abort"}`,
		},
		{
			name: "fragments are wrapped once, with the source named",
			batch: inbox.Batch{
				ChildID: "c_1", Mode: inbox.ModePrompt, Source: "subagents",
				Frags: []string{"a settled", "b settled"},
			},
			id: "F1",
			// json.Marshal escapes <, > and & — as the frame builder this
			// replaced always did. The child's decoder undoes it; the bytes on
			// the wire are what is pinned here.
			want: `{"type":"prompt","id":"F1","message":"\u003crafiki-event source=\"subagents\"\u003e\na settled\nb settled\n\u003c/rafiki-event\u003e"}`,
		},
		{
			name:  "no frame id: nothing to ack, so no id is written",
			batch: inbox.Batch{ChildID: "c_1", Mode: inbox.ModeSteer, Frags: []string{"LOST"}},
			id:    "",
			want:  `{"type":"steer","message":"LOST"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildInjectionFrame(tc.batch, tc.id)
			if err != nil {
				t.Fatalf("buildInjectionFrame: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("frame =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestInboundFromFrameClassifies(t *testing.T) {
	for _, tc := range []struct {
		frame    string
		wantOK   bool
		wantMode inbox.Mode
		wantText string
	}{
		{`{"type":"prompt","message":"hi"}`, true, inbox.ModePrompt, "hi"},
		{`{"type":"steer","message":"stop"}`, true, inbox.ModeSteer, "stop"},
		{`{"type":"abort"}`, true, inbox.ModeAbort, ""},
		{`{"type":"get_state","id":"1"}`, false, 0, ""},
		{`{"type":"extension_ui_response","id":"1"}`, false, 0, ""},
		{`{"type":"new_session","id":"1"}`, false, 0, ""},
		{`not json`, false, 0, ""},
	} {
		in, ok := inboundFromFrame("c_1", json.RawMessage(tc.frame))
		if ok != tc.wantOK {
			t.Fatalf("inboundFromFrame(%s) ok = %v, want %v", tc.frame, ok, tc.wantOK)
		}
		if !ok {
			continue
		}
		if in.Mode != tc.wantMode || in.Text != tc.wantText {
			t.Errorf("inboundFromFrame(%s) = %+v, want mode %v text %q", tc.frame, in, tc.wantMode, tc.wantText)
		}
	}
}

func TestConsumeFramesMapsFrameIdsToRows(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	rec, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	c := &Controller{
		inbox:      inbox.NewQueue(inbox.QueueConfig{Store: st}),
		sentFrames: map[string]sentFrame{"F1": {childID: "c_1", rowIDs: []string{rec.ID}}},
	}
	c.consumeFrames([]string{"F1"})

	if rows, _ := st.Pending(ctx, "c_1"); len(rows) != 0 {
		t.Errorf("the acked row should be terminal, still pending: %+v", rows)
	}
	if _, still := c.sentFrames["F1"]; still {
		t.Error("an acked frame must be forgotten")
	}
}

// consumeRecorder records each MarkConsumed call as a separate set.
//
// The separation is the whole point: Queue.Consume takes the per-child lock on
// the childID it is handed but passes the id slice straight to the store,
// which checks nothing. One call carrying both children's rows means a
// childID was borrowed from the first frame and used to lock child A while
// mutating child B.
type consumeRecorder struct {
	inbox.Store
	mu    sync.Mutex
	calls [][]string
}

func (r *consumeRecorder) MarkConsumed(ctx context.Context, ids []string) error {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), ids...))
	r.mu.Unlock()
	return r.Store.MarkConsumed(ctx, ids)
}

func TestConsumeFramesGroupsByChild(t *testing.T) {
	base := inbox.NewMemory()
	rec := &consumeRecorder{Store: base}
	ctx := context.Background()

	rowFor := func(childID string) string {
		t.Helper()
		r, err := base.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: "hi"})
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if err := base.MarkSent(ctx, []string{r.ID}); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		return r.ID
	}
	rowA, rowB := rowFor("c_a"), rowFor("c_b")

	c := &Controller{
		inbox: inbox.NewQueue(inbox.QueueConfig{Store: rec}),
		sentFrames: map[string]sentFrame{
			"FA": {childID: "c_a", rowIDs: []string{rowA}},
			"FB": {childID: "c_b", rowIDs: []string{rowB}},
		},
	}
	c.consumeFrames([]string{"FA", "FB"})

	rec.mu.Lock()
	calls := rec.calls
	rec.mu.Unlock()

	if len(calls) != 2 {
		t.Fatalf("MarkConsumed calls = %d (%v); want one per child — a single call means "+
			"one child's lock was held while another child's rows were mutated", len(calls), calls)
	}
	want := map[string]string{rowA: "c_a", rowB: "c_b"}
	for _, ids := range calls {
		if len(ids) != 1 {
			t.Fatalf("a per-child call carried %d ids (%v); want 1", len(ids), ids)
		}
		if _, ok := want[ids[0]]; !ok {
			t.Fatalf("unexpected row id %q in call %v", ids[0], ids)
		}
		delete(want, ids[0])
	}
	if len(want) != 0 {
		t.Fatalf("rows never consumed: %v", want)
	}
	for _, child := range []string{"c_a", "c_b"} {
		if rows, _ := base.Pending(ctx, child); len(rows) != 0 {
			t.Errorf("%s still has pending rows: %+v", child, rows)
		}
	}
}

// TestForgetFramesIsScopedToOneChild: the bookkeeping for a dead child goes,
// and nobody else's does. It leaves the ROWS alone — whether they are reset
// (the child may be resumed) or dropped is the caller's decision.
func TestForgetFramesIsScopedToOneChild(t *testing.T) {
	c := &Controller{sentFrames: map[string]sentFrame{
		"FA":  {childID: "c_a", rowIDs: []string{"r1"}},
		"FA2": {childID: "c_a", rowIDs: []string{"r2"}},
		"FB":  {childID: "c_b", rowIDs: []string{"r3"}},
	}}
	c.forgetFrames("c_a")

	if len(c.sentFrames) != 1 {
		t.Fatalf("sentFrames = %+v; want only c_b's entry", c.sentFrames)
	}
	if _, ok := c.sentFrames["FB"]; !ok {
		t.Errorf("another child's unconfirmed frame was forgotten: %+v", c.sentFrames)
	}
}

// acceptRecorder counts what Send submitted to the durable store.
type acceptRecorder struct {
	inbox.Store
	mu   sync.Mutex
	rows []inbox.Inbound
}

func (r *acceptRecorder) Accept(ctx context.Context, in inbox.Inbound) (inbox.Inbound, error) {
	out, err := r.Store.Accept(ctx, in)
	if err == nil {
		r.mu.Lock()
		r.rows = append(r.rows, out)
		r.mu.Unlock()
	}
	return out, err
}

func (r *acceptRecorder) accepted() []inbox.Inbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inbox.Inbound(nil), r.rows...)
}

// TestSendPersistsTurnBoundFramesAndPassesControlFramesThrough is the
// classifier's contract: work for a turn becomes a row, a control frame never
// does. Persisting a control frame would replay a get_state — or worse, a
// cancellation — into an unrelated later turn.
func TestSendPersistsTurnBoundFramesAndPassesControlFramesThrough(t *testing.T) {
	ctrl := newTestController(t)
	rec := &acceptRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	childID := spawnTestChild(t, ctrl, nil)

	if err := ctrl.Send(childID, json.RawMessage(`{"type":"prompt","message":"hi"}`)); err != nil {
		t.Fatalf("Send(prompt): %v", err)
	}
	rows := rec.accepted()
	if len(rows) != 1 || rows[0].Mode != inbox.ModePrompt || rows[0].Text != "hi" {
		t.Fatalf("a prompt must be durably accepted before it is written; got %+v", rows)
	}

	if err := ctrl.Send(childID, json.RawMessage(`{"type":"get_state","id":"g1"}`)); err != nil {
		t.Fatalf("Send(get_state): %v", err)
	}
	if got := rec.accepted(); len(got) != 1 {
		t.Fatalf("a control frame must not be persisted; store now holds %+v", got)
	}
}

// TestSendRefusesADeadChildBeforePersisting: "durably accepted" is a promise.
// Making it for a child that has exited turns a clean error into a row nobody
// will ever consume.
func TestSendRefusesADeadChildBeforePersisting(t *testing.T) {
	ctrl := newTestController(t)
	rec := &acceptRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_gone", Status: protocol.StatusExited, StartedAt: time.Now(),
	})
	err := ctrl.Send("c_gone", json.RawMessage(`{"type":"prompt","message":"hi"}`))
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrChildExited {
		t.Fatalf("Send to an exited child = %v; want a coded child_exited error", err)
	}
	if got := rec.accepted(); len(got) != 0 {
		t.Fatalf("validation must run before persist; store holds %+v", got)
	}
}

// TestOrphanSteerArrivesAsASteer is the degraded path end to end: a PushSteer
// whose durable write failed is delivered without durability, and must still
// be a STEER. A prompt would wait for the next turn — which is the one thing
// an "executor lost" notice cannot do.
//
// Mutation check: hardcode inbox.ModePrompt in deliverOrphans' frame building
// and this test fails on the frame type.
func TestOrphanSteerArrivesAsASteer(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)

	ctrl.flushInboxSource(childID, "executor", []inbox.Inbound{{
		ChildID: childID, Mode: inbox.ModeSteer, Source: "executor", Text: "executor lost",
	}})

	got := waitForChildFrame(t, ctrl, childID, "steer")
	if !strings.Contains(got.Message, "executor lost") {
		t.Errorf("the steer lost its text: %+v", got)
	}
	if !strings.Contains(got.Message, `<rafiki-event source="executor">`) {
		t.Errorf("the orphan lost its source wrapper: %+v", got)
	}
	if got.ID != "" {
		t.Errorf("an orphan has no rows and must not ask for an ack: %+v", got)
	}
}

// TestOrphansCoalesceLikeRows proves the degraded path shares the durable
// path's rules rather than reimplementing them: last-write-wins on the key,
// and any steer in the group makes the whole batch a steer.
func TestOrphansCoalesceLikeRows(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)

	ctrl.flushInboxSource(childID, "subagents", []inbox.Inbound{
		{ChildID: childID, Mode: inbox.ModePrompt, Source: "subagents", Key: "c_w1", Text: "w1 first"},
		{ChildID: childID, Mode: inbox.ModePrompt, Source: "subagents", Key: "c_w1", Text: "w1 second"},
		{ChildID: childID, Mode: inbox.ModeSteer, Source: "subagents", Text: "budget exhausted"},
	})

	got := waitForChildFrame(t, ctrl, childID, "steer")
	if strings.Contains(got.Message, "w1 first") {
		t.Errorf("a superseded fragment survived: %s", got.Message)
	}
	for _, want := range []string{"w1 second", "budget exhausted"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("fragment %q missing from %s", want, got.Message)
		}
	}
}

// --- helpers ---

// childFrame is the child-facing injection frame, decoded.
type childFrame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

// waitForChildFrame polls the child's captured stdin frames for one of the
// given type and returns it decoded.
//
// Child.InSnapshot records the ORIGINAL normalized frame, before the
// provider's EncodeOutbound, which is what makes the frame's type observable
// here at all: a claude child re-encodes prompt and steer to the same user
// envelope on its way to stdin.
func waitForChildFrame(t *testing.T, ctrl *Controller, childID, frameType string) childFrame {
	t.Helper()
	ch, ok := ctrl.cm.Get(childID)
	if !ok {
		t.Fatalf("child %s is not live", childID)
	}
	deadline := time.Now().Add(5 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		seen = nil
		for _, raw := range ch.InSnapshot() {
			seen = append(seen, string(raw))
			var f childFrame
			if json.Unmarshal(raw, &f) == nil && f.Type == frameType {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s frame reached the child; it saw %v", frameType, seen)
	return childFrame{}
}

// fakeClaudeScript writes a claude stand-in that announces a session and then
// blocks on stdin. It does NOT trap SIGINT, so the interrupt actually kills it,
// and it reports "got-<arg>" when --resume threaded a session id through —
// which is what makes "the interrupt path ran" observable rather than assumed.
func fakeClaudeScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	body := "#!/bin/bash\n" +
		"SID=fresh\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"--resume\" ]; then SID=\"got-$a\"; fi; prev=\"$a\"; done\n" +
		"printf '%s\\n' \"{\\\"type\\\":\\\"system\\\",\\\"subtype\\\":\\\"init\\\",\\\"session_id\\\":\\\"$SID\\\",\\\"model\\\":\\\"claude-opus-4-8\\\"}\"\n" +
		"while IFS= read -r line; do :; done\n" +
		"while true; do sleep 0.05; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return script
}

// TestConnectAbortToAClaudeChildIsNeverPersisted closes the hole the durable
// inbox opened in the Connect face: Server.Send goes straight to the Accepter,
// so an accepter that is the bare queue skips Controller.Send's classifier
// entirely and a claude abort becomes a ROW. A crash or a failed delivery
// between Accept and delivery would leave that row pending, and the idle drain
// would later deliver a stored cancellation as SIGINT+resume into an unrelated
// turn — the exact hazard "classify before persisting" exists to prevent.
//
// Mutation check: swap ctrl.connectInbox() for ctrl.inbox below and this test
// fails on the row count.
func TestConnectAbortToAClaudeChildIsNeverPersisted(t *testing.T) {
	ctrl, childID := newClaudeTestChild(t, fakeClaudeScript(t))
	rec := &acceptRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	before, ok := ctrl.cm.Get(childID)
	if !ok {
		t.Fatalf("child %s is not live", childID)
	}
	pidBefore := before.PID()

	srv := connectapi.NewServer(nil)
	srv.SetInbox(ctrl.connectInbox())

	resp, err := srv.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_ABORT,
	}))
	if err != nil {
		t.Fatalf("Send(ABORT): %v", err)
	}
	if got := rec.accepted(); len(got) != 0 {
		t.Fatalf("a claude abort must never become a row; store holds %+v", got)
	}
	// No row means no id to quote back. An invented one would resolve to
	// nothing in every store.
	if id := resp.Msg.GetMessageId(); id != "" {
		t.Errorf("message_id = %q; want empty — there is no row to name", id)
	}

	// And it must still abort: the interrupt path replaces the process under
	// the same child id.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := ctrl.cm.Get(childID); ok && ch.PID() != pidBefore && ch.Status() == protocol.StatusIdle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the abort did not reach the interrupt path: no resumed process under the same child id")
}

// TestConnectAbortToAFundiChildStillQueues pins the other half: the diversion
// is scoped to the claude kind. A fundi child speaks abort natively, so its
// abort is an ordinary durable message and must be persisted and delivered.
//
// The child's KIND is what the classifier reads, so the fixture sets it
// directly on the store snapshot rather than paying for a real fundi spawn
// (which re-execs rafikid and needs a model); the live process underneath only
// has to accept a frame.
func TestConnectAbortToAFundiChildStillQueues(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)
	if err := ctrl.st.Update(childID, func(s *childstore.Session) { s.Kind = protocol.KindFundi }); err != nil {
		t.Fatalf("relabel kind: %v", err)
	}
	rec := &acceptRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	srv := connectapi.NewServer(nil)
	srv.SetInbox(ctrl.connectInbox())

	resp, err := srv.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_ABORT,
	}))
	if err != nil {
		t.Fatalf("Send(ABORT): %v", err)
	}
	rows := rec.accepted()
	if len(rows) != 1 || rows[0].Mode != inbox.ModeAbort {
		t.Fatalf("a fundi abort must be durably accepted; store holds %+v", rows)
	}
	if resp.Msg.GetMessageId() != rows[0].ID {
		t.Errorf("message_id = %q; want the row id %q", resp.Msg.GetMessageId(), rows[0].ID)
	}
	// And it was DELIVERED, not merely stored: an abort awaits no ack, so a
	// successful write retires the row on the spot. A row still pending would
	// mean Accept persisted and nothing ever wrote it to the child.
	if pending, err := rec.Pending(context.Background(), childID); err != nil || len(pending) != 0 {
		t.Fatalf("row still pending after Send (err=%v): %+v — it was queued but never delivered", err, pending)
	}
}

// --- lifecycle: drain on idle, reset on exit, drop on forget, sweep on the tick ---

// TestExitResetsSentRowsRatherThanDroppingThem pins the asymmetry the whole
// lifecycle rests on. An exit is NOT the moment a row becomes unrunnable: the
// child can be resumed, and its queue is exactly what a resume should run.
//
// Mutation check: swap Reset for Drop in releaseInboxOnExit and this fails.
func TestExitResetsSentRowsRatherThanDroppingThem(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	rec, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "work"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})
	c.sentFrames = map[string]sentFrame{"F1": {childID: "c_1", rowIDs: []string{rec.ID}}}
	c.releaseInboxOnExit("c_1")

	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("an exited child's unconfirmed row must return to pending so a resume can run it; got %d", len(rows))
	}
	if len(c.sentFrames) != 0 {
		t.Errorf("the dead child's frame bookkeeping must go with it: %+v", c.sentFrames)
	}
}

// TestForgetDropsTheQueue is the other half: forget is the one moment there
// will never be a turn again, so the rows terminate.
func TestForgetDropsTheQueue(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "work"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})
	c.dropInboxForForgotten("c_1", "child forgotten")

	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a forgotten child's queue must be dropped; %d rows survived", len(rows))
	}
	// Dropped, not merely un-pending: a terminal row is what the retention
	// sweep can reach. A row left in any other state leaks forever.
	if n, err := st.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("swept %d rows (err=%v); want 1 — the dropped row must be terminal", n, err)
	}
}

// TestForgetDoesNotDropInboxForAChildAnotherDaemonOwns is the multi-daemon
// trap in Forget's own inbox drop: loadChildren inserts every daemon's
// children into this daemon's local store as exited, including a child that
// is genuinely alive on ANOTHER daemon right now, and recoverOne writes
// exactly that placeholder status BEFORE its async resume goroutine even
// runs. A Forget landing in that window must not act on the local snapshot
// alone -- ownsChildRow, the same authority the row-delete a few lines below
// already uses, is what recognises the row is not this daemon's to destroy.
//
// Mutation check: drop the ownsChildRow gate in Forget and this fails --
// the row is a Drop (terminal), which is worse than the reset-to-pending the
// lease-ownership bug caused, and there is no future resume that can ever
// revive a dropped row.
func TestForgetDoesNotDropInboxForAChildAnotherDaemonOwns(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "work"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})
	c.st.Insert(&childstore.Session{
		ChildID: "c_1",
		Status:  protocol.StatusExited,
		Labels:  map[string]string{"rafiki/daemon": "some-other-daemon"},
	})

	if err := c.Close("c_1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a child owned by another daemon must keep its queue intact; %d rows survived, want 1", len(rows))
	}
}

// TestForgetDropsInboxForAnOwnedChild is the other half: an ordinary forget
// of a child this daemon actually owns must still drop its queue. A gate
// here that is too aggressive leaks rows silently forever rather than
// failing loudly, which is why this is asserted as its own test rather than
// inferred from the cross-daemon case above.
func TestForgetDropsInboxForAnOwnedChild(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "work"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})
	c.st.Insert(&childstore.Session{
		ChildID: "c_1",
		Status:  protocol.StatusExited,
		// No rafiki/daemon label: ownsChildRow treats an unlabelled row as
		// ours (it predates the label), same as one this daemon labelled
		// itself.
	})

	if err := c.Close("c_1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("an owned, forgotten child's queue must be dropped; %d rows survived", len(rows))
	}
}

// TestForgetAllExitedSkipsInboxForAnUnownedChildButDropsForAnOwnedOne mirrors
// the Forget-level pair above for the sweep entry point: sweepExpired calls
// ForgetAllExited on the daemon's own grace-window tick, and a recovered
// record -- including one belonging to a still-live OTHER daemon's child --
// carries its old ExitedAt, so this path reaches the identical trap on a
// timer rather than needing a client to call Forget at the wrong moment.
func TestForgetAllExitedSkipsInboxForAnUnownedChildButDropsForAnOwnedOne(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	for _, id := range []string{"c_mine", "c_theirs"} {
		if _, err := st.Accept(ctx, inbox.Inbound{ChildID: id, Mode: inbox.ModePrompt, Text: "work"}); err != nil {
			t.Fatalf("Accept %s: %v", id, err)
		}
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})
	c.st.Insert(&childstore.Session{ChildID: "c_mine", Status: protocol.StatusExited})
	c.st.Insert(&childstore.Session{
		ChildID: "c_theirs",
		Status:  protocol.StatusExited,
		Labels:  map[string]string{"rafiki/daemon": "some-other-daemon"},
	})

	n, err := c.CloseAllExited(0)
	if err != nil {
		t.Fatalf("ForgetAllExited: %v", err)
	}
	if n != 2 {
		t.Fatalf("ForgetAllExited count = %d, want 2 (both are still forgotten locally)", n)
	}

	mineRows, err := st.Pending(ctx, "c_mine")
	if err != nil {
		t.Fatalf("Pending c_mine: %v", err)
	}
	if len(mineRows) != 0 {
		t.Fatalf("an owned child's queue must be dropped; %d rows survived", len(mineRows))
	}

	theirsRows, err := st.Pending(ctx, "c_theirs")
	if err != nil {
		t.Fatalf("Pending c_theirs: %v", err)
	}
	if len(theirsRows) != 1 {
		t.Fatalf("a child owned by another daemon must keep its queue intact; %d rows survived, want 1", len(theirsRows))
	}
}

// TestHandleChildExitResetsTheInbox proves the hook is wired at the exit site,
// not merely that the helper works.
func TestHandleChildExitResetsTheInbox(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)
	st := inbox.NewMemory()
	ctrl.inbox = ctrl.newInboxQueue(st)

	ctx := context.Background()
	row, err := st.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: "work"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{row.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	if _, err := ctrl.Kill(context.Background(), childID, 1000, 500); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, _ := st.Pending(ctx, childID); len(rows) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := st.Pending(ctx, childID)
	t.Fatalf("the exit path never returned the unconfirmed row to pending; pending=%+v", rows)
}

// TestHandleChildExitResetsInboxWithNoDatabase pins I3 from the final
// review: the exit gate's ownership check must not apply when there is no
// lease system at all. c.leases is nil exactly when there is no database
// pool (set only in NewController's pool != nil block, the same block that
// sets c.children) -- the DEFAULT dev configuration, which this repo's own
// newTestController builds (a nil pool). Without a c.leases == nil
// disjunct in the gate, OnConversationResolved's own short-circuit means no
// fundi child under such a daemon ever calls trackLease, so holdsLease is
// PERMANENTLY false and this reset was skipped on EVERY fundi exit --
// silently stranding rows at 'sent' (invisible to the pending-only idle
// drain) and logging a false "never held the lease" WARN every time.
//
// Uses a real, in-process fundi child (not spawnTestChild's claude/fake-pi
// subprocess) because Kind must genuinely be KindFundi for this test to
// exercise the disjunct under test rather than the (already-covered)
// claude/pi exemption.
func TestHandleChildExitResetsInboxWithNoDatabase(t *testing.T) {
	ctrl := newTestController(t)
	if ctrl.leases != nil {
		t.Fatal("test assumes no database pool; ctrl.leases must be nil")
	}

	got, err := ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Type:      protocol.TypeCtrlSpawn,
		Kind:      protocol.KindFundi,
		Model:     "anthropic/sonnet-latest",
		Cwd:       t.TempDir(),
		NoSession: true,
	}, users.Identity{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	childID := got.ChildID

	st := inbox.NewMemory()
	ctrl.inbox = ctrl.newInboxQueue(st)

	ctx := context.Background()
	row, err := st.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: "work"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{row.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	if _, err := ctrl.Kill(context.Background(), childID, 1000, 500); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, _ := st.Pending(ctx, childID); len(rows) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := st.Pending(ctx, childID)
	t.Fatalf("a fundi child's exit under a no-database daemon must still reset its inbox; pending=%+v", rows)
}

// TestDrainInboxDeliversDirectMessagesOnly pins I4 from the final review:
// the idle-transition drain (drainInbox) must retry only DIRECT messages
// (source ""), never fragment-sourced rows -- those are eventbuf's to
// schedule, and handleStatusChange already calls evbuf.DrainIdle, which
// releases only deferred-and-dirty keys, right before this runs. DeliverAll's
// Pending() read has no concept of "still inside its debounce window": it
// returns every pending row regardless of source, so calling it here executed
// exactly the delivery DrainIdle had just refused, one line later, defeating
// the coalescing eventbuf exists for.
func TestDrainInboxDeliversDirectMessagesOnly(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Source: "", Text: "direct message"}); err != nil {
		t.Fatalf("Accept direct: %v", err)
	}
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Source: "subagents", Key: "c_2", Text: "fragment, still debouncing"}); err != nil {
		t.Fatalf("Accept fragment: %v", err)
	}

	var delivered []inbox.Batch
	q := inbox.NewQueue(inbox.QueueConfig{
		Store: st,
		Deliver: func(_ context.Context, b inbox.Batch) (bool, error) {
			delivered = append(delivered, b)
			return false, nil
		},
	})

	c := newTestController(t)
	c.drainInbox(q, "c_1")

	if len(delivered) != 1 {
		t.Fatalf("delivered %d batches, want 1 (direct only)", len(delivered))
	}
	if delivered[0].Source != "" || delivered[0].Frags[0] != "direct message" {
		t.Fatalf("delivered batch = %+v; want the direct message, not the still-debouncing fragment", delivered[0])
	}

	// The fragment must still be pending -- untouched, ready for eventbuf's
	// own debounce/DrainIdle to release it in its own time.
	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 || rows[0].Source != "subagents" {
		t.Fatalf("pending after drainInbox = %+v; want the fragment still pending, untouched", rows)
	}
}

// TestForgetPathsDropTheQueue covers BOTH deletion paths. ForgetAllExited is a
// separate loop from Forget, and a row for a child forgotten through it is
// never pending-for-a-live-child again and never terminal — so the retention
// sweep can never reach it and it leaks permanently.
//
// Mutation check: remove either hook and its subtest fails.
func TestForgetPathsDropTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forget func(t *testing.T, ctrl *Controller, childID string)
	}{
		{
			name: "Forget",
			forget: func(t *testing.T, ctrl *Controller, childID string) {
				if err := ctrl.Close(childID); err != nil {
					t.Fatalf("Forget: %v", err)
				}
			},
		},
		{
			name: "ForgetAllExited",
			forget: func(t *testing.T, ctrl *Controller, childID string) {
				n, err := ctrl.CloseAllExited(0)
				if err != nil || n != 1 {
					t.Fatalf("ForgetAllExited = %d, %v; want 1, nil", n, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := newTestController(t)
			childID := spawnTestChild(t, ctrl, nil)
			st := inbox.NewMemory()
			ctrl.inbox = ctrl.newInboxQueue(st)

			if _, err := ctrl.Kill(context.Background(), childID, 1000, 500); err != nil {
				t.Fatalf("Kill: %v", err)
			}

			// Queued AFTER the exit: the store is shared across daemons, so a
			// row can arrive for a child this daemon has already buried.
			ctx := context.Background()
			if _, err := st.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: "work"}); err != nil {
				t.Fatalf("Accept: %v", err)
			}

			tc.forget(t, ctrl, childID)

			if rows, _ := st.Pending(ctx, childID); len(rows) != 0 {
				t.Fatalf("a forgotten child's queue must be dropped; %d rows survived", len(rows))
			}
			if n, err := st.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
				t.Fatalf("swept %d rows (err=%v); want 1 — a row left non-terminal is one "+
					"the retention sweep can never reach", n, err)
			}
		})
	}
}

// drainRecorder captures what the queue was asked to deliver.
//
// The drain tests build the child by hand rather than spawning one: a real
// spawn drives its own idle transition, so its drain goroutine is still in
// flight when the test inserts a row, and either test could then pass or fail
// on that rather than on the transition it means to exercise.
type drainRecorder struct {
	mu      sync.Mutex
	batches []inbox.Batch
}

func (r *drainRecorder) deliver(_ context.Context, b inbox.Batch) (bool, error) {
	r.mu.Lock()
	r.batches = append(r.batches, b)
	r.mu.Unlock()
	return false, nil
}

func (r *drainRecorder) seen() []inbox.Batch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inbox.Batch(nil), r.batches...)
}

// idleDrainFixture wires a controller to an idle child that no process backs,
// with one pending row waiting for it.
func idleDrainFixture(t *testing.T, text string) (*Controller, *drainRecorder, *inbox.Memory, string) {
	t.Helper()
	ctrl := newTestController(t)
	const childID = "c_drain"
	ctrl.st.Insert(&childstore.Session{
		ChildID: childID,
		Kind:    protocol.KindClaude,
		Status:  protocol.StatusIdle,
		Cwd:     t.TempDir(),
	})
	st := inbox.NewMemory()
	rec := &drainRecorder{}
	ctrl.inbox = inbox.NewQueue(inbox.QueueConfig{
		Store:    st,
		Validate: ctrl.validateSendTarget,
		Deliver:  rec.deliver,
		Batch:    ctrl.inboxBatch,
	})
	if _, err := st.Accept(context.Background(), inbox.Inbound{
		ChildID: childID, Mode: inbox.ModePrompt, Text: text,
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return ctrl, rec, st, childID
}

// TestIdleTransitionDrainsTheInbox is the retry path: an immediate delivery
// that failed leaves the row pending, and the child's next idle transition is
// what picks it up.
func TestIdleTransitionDrainsTheInbox(t *testing.T) {
	ctrl, rec, _, childID := idleDrainFixture(t, "left behind")
	if _, ok := ctrl.st.SetStatus(childID, protocol.StatusStreaming); !ok {
		t.Fatalf("SetStatus: child %s not in store", childID)
	}

	ctrl.handleStatusChange(childID, protocol.StatusIdle, protocol.StatusStreaming)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.seen(); len(got) == 1 {
			if len(got[0].Frags) != 1 || got[0].Frags[0] != "left behind" {
				t.Fatalf("the drained batch is not the pending row: %+v", got[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the idle transition never drained the pending row")
}

// TestNonIdleTransitionDoesNotDrainTheInbox is the other half, and it is not
// paranoia: eventbuf's fragments are inbox rows, persisted on Push and held
// pending while the buffer debounces and while the child is mid-turn. A drain
// on every status change would deliver them the instant the child started
// working -- defeating the debounce and the busy gate at once.
//
// Mutation check: drop the `newStatus == StatusIdle` condition and this fails.
func TestNonIdleTransitionDoesNotDrainTheInbox(t *testing.T) {
	ctrl, rec, st, childID := idleDrainFixture(t, "not yet")

	ctrl.handleStatusChange(childID, protocol.StatusStreaming, protocol.StatusIdle)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := rec.seen(); len(got) != 0 {
			t.Fatalf("a pending row was delivered on a transition INTO a working status; "+
				"the buffer's debounce and busy gate are both bypassed: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows, _ := st.Pending(context.Background(), childID); len(rows) != 1 {
		t.Fatalf("the row should still be pending; got %d", len(rows))
	}
}

// sweepRecorder captures the cutoff the retention sweep asks for.
type sweepRecorder struct {
	inbox.Store
	mu      sync.Mutex
	cutoffs []time.Time
}

func (r *sweepRecorder) Sweep(ctx context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	r.cutoffs = append(r.cutoffs, before)
	r.mu.Unlock()
	return r.Store.Sweep(ctx, before)
}

func (r *sweepRecorder) seen() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.cutoffs...)
}

// TestSweepTickSweepsTheInboxWithTheRetentionWindow pins the fourth hook to
// the tick that already exists, and pins the cutoff it asks for. A sweep with
// a "now" cutoff would delete a row the operator is still asking about.
func TestSweepTickSweepsTheInboxWithTheRetentionWindow(t *testing.T) {
	ctrl := newTestController(t)
	rec := &sweepRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	before := time.Now()
	ctrl.sweepTick(t.Context())

	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("Sweep calls = %d; want exactly 1 from the periodic tick", len(got))
	}
	want := before.Add(-inboxRetention)
	if d := got[0].Sub(want); d < 0 || d > 5*time.Second {
		t.Errorf("sweep cutoff = %v; want ~%v (now - inboxRetention)", got[0], want)
	}
}

// TestStaleClaudeAbortRowIsRetiredNotReplayed closes the last path by which a
// lock-holding delivery could wait on monitorChild.
//
// Queue.deliver holds the per-child lock across deliverInbox -> sendFrame, and
// sendFrame routes an abort aimed at a claude child into handleClaudeAbort:
// SIGINT, poll for the exit, respawn. handleChildExit takes that same lock to
// reset the child's unconfirmed rows, so the abort would spin out its full
// wait for a cm.Remove that cannot happen until it lets go — and then resume a
// child the blocked exit path is about to remove from the manager.
//
// Nothing creates such a row today (Controller.Send and connectAccepter both
// classify before persisting), which is exactly why the guard belongs at the
// one point every delivery funnels through rather than at a third call site.
func TestStaleClaudeAbortRowIsRetiredNotReplayed(t *testing.T) {
	ctrl, childID := newClaudeTestChild(t, fakeClaudeScript(t))
	st := inbox.NewMemory()
	ctrl.inbox = ctrl.newInboxQueue(st)

	before, ok := ctrl.cm.Get(childID)
	if !ok {
		t.Fatalf("child %s is not live", childID)
	}
	pidBefore := before.PID()

	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModeAbort}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := ctrl.inbox.DeliverAll(ctx, childID); err != nil {
		t.Fatalf("DeliverAll: %v", err)
	}

	if rows, _ := st.Pending(ctx, childID); len(rows) != 0 {
		t.Fatalf("the stale abort must be retired, not left to be retried forever: %+v", rows)
	}
	if n, err := st.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("swept %d rows (err=%v); want 1 — the retired row must be terminal", n, err)
	}
	if ch, ok := ctrl.cm.Get(childID); !ok || ch.PID() != pidBefore {
		t.Fatal("the stored abort reached the interrupt path: the process was replaced")
	}
}
