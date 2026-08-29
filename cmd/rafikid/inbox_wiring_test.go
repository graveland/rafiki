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
