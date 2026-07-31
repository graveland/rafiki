package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/protocol"
)

// burstTurns is the number of complete idle→streaming→idle turns the fake pi
// emits in one write for the transition-loss test. Each turn is two
// transitions, so the expected event count is 2*burstTurns.
const burstTurns = 10

// statusEvents returns every ctrl_child_status frame conn has collected, in
// delivery order.
func statusEvents(conn *collectConn) []protocol.CtrlChildStatus {
	conn.mu.Lock()
	frames := make([][]byte, len(conn.frames))
	copy(frames, conn.frames)
	conn.mu.Unlock()
	return statusEventsIn(frames)
}

// framesUntilExit returns the frames conn collected up to (excluding) the first
// ctrl_child_exited, plus that exit event — or (all frames, nil) if the child
// has not been reported exited yet.
func framesUntilExit(conn *collectConn) ([][]byte, *protocol.CtrlChildExited) {
	conn.mu.Lock()
	frames := make([][]byte, len(conn.frames))
	copy(frames, conn.frames)
	conn.mu.Unlock()

	for i, f := range frames {
		var evt protocol.CtrlChildExited
		if err := json.Unmarshal(f, &evt); err == nil && evt.Type == protocol.TypeCtrlChildExited {
			return frames[:i], &evt
		}
	}
	return frames, nil
}

// statusEventsIn extracts the ctrl_child_status frames from frames, in order.
func statusEventsIn(frames [][]byte) []protocol.CtrlChildStatus {
	var out []protocol.CtrlChildStatus
	for _, f := range frames {
		var evt protocol.CtrlChildStatus
		if err := json.Unmarshal(f, &evt); err != nil {
			continue
		}
		if evt.Type == protocol.TypeCtrlChildStatus {
			out = append(out, evt)
		}
	}
	return out
}

// TestMonitorChild_FastTurnBurst_LosesNoTransition is the regression test for
// status frames being derived by SAMPLING rather than recorded at the moment
// they happen.
//
// The state machine transitions on the child's readStdout goroutine, which runs
// far ahead of monitorChild's consumption of the bus: monitorChild does a store
// lookup and three subscriber fan-outs per frame, readStdout does a JSON header
// decode. So when a turn's frames arrive in one burst, the child completes its
// entire idle→streaming→idle round trip between two of monitorChild's samples,
// and BOTH transitions vanish — a live subscriber watching the child sees
// nothing at all for a fast turn.
//
// Ten turns in a single write makes that deterministic enough to assert on: a
// sampling monitor cannot report 20 transitions it never looked in time to see.
// The assertion is the exact alternating sequence, which pins both halves of
// the contract at once — nothing lost, and nothing delivered twice.
func TestMonitorChild_FastTurnBurst_LosesNoTransition(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, map[string]string{
		"FAKE_PI_BURST_TURNS": "10",
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ctrl.ShutdownAllChildren(ctx, 2*time.Second, time.Second); err != nil {
			t.Errorf("cleanup ShutdownAllChildren: %v", err)
		}
	})

	// Subscribe AFTER spawn so the spawning→idle transition (emitted
	// synchronously by activateLiveChild) is not part of what we count.
	conn := &collectConn{}
	ctrl.cm.GlobalSubscribe(conn)
	t.Cleanup(func() { ctrl.cm.GlobalUnsubscribe(conn) })

	if err := ctrl.Send(childID, json.RawMessage(`{"type":"__ctrl_test_burst"}`)); err != nil {
		t.Fatalf("send burst: %v", err)
	}

	want := 2 * burstTurns
	deadline := time.Now().Add(5 * time.Second)
	var got []protocol.CtrlChildStatus
	for time.Now().Before(deadline) {
		got = statusEvents(conn)
		if len(got) >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give any duplicate a chance to land before asserting on the count, so a
	// double-delivery bug fails loudly here instead of passing by arriving late.
	time.Sleep(200 * time.Millisecond)
	got = statusEvents(conn)

	if len(got) != want {
		t.Fatalf("got %d ctrl_child_status events, want %d (%d turns x 2 transitions): %+v",
			len(got), want, burstTurns, got)
	}
	for i, evt := range got {
		wantFrom, wantTo := protocol.StatusIdle, protocol.StatusStreaming
		if i%2 == 1 {
			wantFrom, wantTo = protocol.StatusStreaming, protocol.StatusIdle
		}
		if evt.Previous != string(wantFrom) || evt.Status != string(wantTo) {
			t.Errorf("event %d = %s→%s, want %s→%s", i, evt.Previous, evt.Status, wantFrom, wantTo)
		}
		if evt.ChildID != childID {
			t.Errorf("event %d childId = %q, want %q", i, evt.ChildID, childID)
		}
	}

	// The store must land on the terminal status of the burst, not on whatever
	// a sample happened to catch.
	if snap, ok := ctrl.st.Get(childID); !ok || snap.Status != protocol.StatusIdle {
		t.Errorf("store status = %v (found=%v), want idle", snap.Status, ok)
	}
}

// TestMonitorChild_BurstThenExit_DeliversTransitionsBeforeExit covers the exit
// path: the child writes a full turn's frames and dies in the same breath, so
// the process-death signal races the frames it just wrote.
//
// Both of monitorChild's exit branches therefore drain queued transitions
// BEFORE handleChildExit. After handleChildExit the transitions would be
// undeliverable (cm.Remove clears the per-child subscriber list) as well as
// out of order behind ctrl_child_exited — and last_status on the exit event,
// which is read from the store, would report a status the child had already
// left.
func TestMonitorChild_BurstThenExit_DeliversTransitionsBeforeExit(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, map[string]string{
		"FAKE_PI_BURST_TURNS":     "10",
		"FAKE_PI_BURST_THEN_EXIT": "1",
	})

	conn := &collectConn{}
	ctrl.cm.GlobalSubscribe(conn)
	t.Cleanup(func() { ctrl.cm.GlobalUnsubscribe(conn) })

	if err := ctrl.Send(childID, json.RawMessage(`{"type":"__ctrl_test_burst"}`)); err != nil {
		t.Fatalf("send burst: %v", err)
	}

	// Wait for the exit EVENT, not for the store: handleChildExit calls
	// MarkExited before it delivers ctrl_child_exited, so a store poll can
	// outrun the delivery. Waiting for the event is also the stronger claim —
	// everything the drain owed must already be in front of it.
	var exited *protocol.CtrlChildExited
	var frames [][]byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frames, exited = framesUntilExit(conn)
		if exited != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exited == nil {
		t.Fatal("no ctrl_child_exited event delivered")
	}

	got := statusEventsIn(frames)
	if len(got) != 2*burstTurns {
		t.Fatalf("got %d ctrl_child_status events before ctrl_child_exited, want %d — a transition recorded just before exit was dropped: %+v",
			len(got), 2*burstTurns, got)
	}
	if last := got[len(got)-1]; last.Status != string(protocol.StatusIdle) {
		t.Errorf("final transition = %s→%s, want ...→idle", last.Previous, last.Status)
	}
	// The exit event must carry the drained final status, not a stale one.
	if exited.LastStatus != string(protocol.StatusIdle) {
		t.Errorf("ctrl_child_exited last_status = %q, want %q", exited.LastStatus, protocol.StatusIdle)
	}
}

// TestSpawn_StalledChild_EmitsNoStatusTransition pins the spawn path's stalled
// outcome, which lost its explicit `if !stalled` guard when the spawning→idle
// event stopped being asserted by hand and started being drained from what the
// child actually recorded.
//
// A child that never answers the get_state probe never leaves spawning, so it
// records no transition and the drain must emit nothing: the record stays
// spawning and Stalled is reported to the caller. The guard is redundant, not
// missing — and dropping it is what lets a child that reaches idle just after
// the timeout expired be reported correctly rather than being left as spawning
// forever, which is what the old hardcoded pair did.
func TestSpawn_StalledChild_EmitsNoStatusTransition(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	// Subscribe BEFORE spawning: the spawn path emits its status event
	// synchronously, so a late subscriber could not tell silence from a miss.
	conn := &collectConn{}
	ctrl.cm.GlobalSubscribe(conn)
	t.Cleanup(func() { ctrl.cm.GlobalUnsubscribe(conn) })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ctrl.ShutdownAllChildren(ctx, 2*time.Second, time.Second); err != nil {
			t.Errorf("cleanup ShutdownAllChildren: %v", err)
		}
	})

	// activateLiveChild's idle wait is 5s, so allow well past it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
		Env:       map[string]string{"FAKE_PI_IGNORE_GET_STATE": "1"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !res.Stalled {
		t.Fatalf("Stalled = false, want true for a child that never answers get_state")
	}

	if got := statusEvents(conn); len(got) != 0 {
		t.Errorf("got %d ctrl_child_status events for a stalled spawn, want 0: %+v", len(got), got)
	}
	if snap, ok := ctrl.st.Get(res.ChildID); !ok || snap.Status != protocol.StatusSpawning {
		t.Errorf("store status = %v (found=%v), want spawning", snap.Status, ok)
	}
}
