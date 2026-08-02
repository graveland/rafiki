package fundi

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// capturedLogs redirects the default slog to a buffer for the duration of the
// test and returns it. Call it AFTER newTestEngine*, whose silenceSlog sends
// logs to io.Discard: cleanups unwind LIFO, so this handler is the one in force
// during the test body and the original default is still restored at the end.
func capturedLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestEngineAbortRunsTheAbortBranch is the first test in this repo to execute
// Engine.runTurn's abort arm at all.
//
// It could not exist before fakeSender/capturingSender honoured their context.
// rafiki's agentloop.drive does not check ctx.Err() between iterations, so a
// context-blind fake sender answered one more iteration after the abort, the
// turn completed with err == nil, and runTurn took NEITHER error branch — the
// aborted arm, RepairOrphans included, was dead code from every test's point of
// view. RepairOrphans is the thing that stops the NEXT API call being rejected
// for a dangling tool_use, in the subsystem whose own comment calls in-band
// abort "this project's entire reason for existing", so "never executed by a
// test" was not a gap worth keeping.
//
// What this asserts, in order of what would actually break in production:
//
//  1. The abort arm runs — witnessed by its own log line, which no other path
//     emits, and by the ABSENCE of the agent_error frame the plain-error arm
//     emits. Getting one and not the other is what distinguishes "aborted" from
//     "failed" and from "succeeded".
//  2. RepairOrphans runs inside it, reporting its repair count.
//  3. The aborted iteration issues NO further API call, so the next scripted
//     turn is still there — the concrete bug the context-blind sender caused.
//  4. The next prompt SUCCEEDS, and the request it puts on the wire is the
//     shape the real API accepts: every tool_use followed by a tool_result.
//     Asserted against the captured outgoing params, not inferred from the
//     scripted call returning.
//
// On the repair COUNT: it is 0 here, and that is the only correct expectation
// for a store-less conversation. agentloop persists each tool result through
// conv.AppendUser, and the in-memory conversation path ignores ctx entirely, so
// the result lands even on a cancelled turn and there is no orphan left to
// repair. A genuine orphan needs a store that honours ctx (pgx does), which is
// exactly what TestRepairOrphansDBBackedGenuineOrphan covers end to end,
// including the repaired request shape. Asserting 0 here is deliberate: it pins
// that RepairOrphans ran and found the history already consistent, rather than
// quietly passing on whatever number turned up.
func TestEngineAbortRunsTheAbortBranch(t *testing.T) {
	toolRunning := make(chan struct{})
	var once sync.Once
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		once.Do(func() { close(toolRunning) })
		// The abort is the ONLY thing that can end this tool: no wall-clock
		// margin, so a test that forgot to abort would hang rather than pass
		// for the wrong reason.
		<-ctx.Done()
		return "", ctx.Err()
	}}

	// sampleResp is the tool_use turn (tool "bash", id tu_1); sampleEndTurn is
	// left for the SECOND prompt, and the assertion that it is still available
	// is assertion 3.
	sender := newCapturingSender(t, sampleResp, sampleEndTurn)
	eng, out := newTestEngineWithSender(t, ts, sender)
	logs := capturedLogs(t)

	eng.HandlePrompt("go")
	select {
	case <-toolRunning:
	case <-time.After(10 * time.Second):
		t.Fatal("the scripted tool never started; there is nothing to abort")
	}
	eng.HandleAbort()
	eng.Wait()

	// (1) The abort arm ran, and the plain-error arm did not.
	logText := logs.String()
	if !strings.Contains(logText, "agent: turn cancelled") {
		t.Errorf("no %q log line; runTurn did not take its abort branch.\nlogs:\n%s",
			"agent: turn cancelled", logText)
	}
	if strings.Contains(logText, "agent: turn failed") {
		t.Errorf("runTurn took the plain-error branch instead of the abort branch.\nlogs:\n%s", logText)
	}
	turn1 := frameTypes(t, out.String())
	for _, ty := range turn1 {
		if ty == "agent_error" {
			t.Fatalf("an aborted turn emitted agent_error; that frame belongs to the plain-error branch only: %v", turn1)
		}
	}

	// (2) RepairOrphans ran inside the abort arm and reported its count. See
	// this test's doc comment for why 0 is the right number store-less.
	if !strings.Contains(logText, "orphans_repaired=0") {
		t.Errorf("abort branch did not report an orphan-repair count of 0; RepairOrphans may not have run.\nlogs:\n%s", logText)
	}

	// (3) The aborted iteration issued no API call, so the second scripted turn
	// was not consumed. Exactly one request for turn 1: the iteration that
	// produced the tool_use.
	if got := sender.callCount(); got != 1 {
		t.Fatalf("sender served %d requests during the aborted turn, want 1; "+
			"a context-blind sender answers the post-abort iteration and eats the next scripted turn", got)
	}
	// One assistant message in turn 1, not two: no second AssistantTurn was
	// rendered from a scripted reply the abort should have prevented.
	if got := countFrames(turn1, "message_end"); got != 2 {
		t.Errorf("turn 1 emitted %d message_end frames, want 2 (user echo + one assistant turn): %v", got, turn1)
	}

	// (4) The next prompt succeeds, on the same engine, and the request it
	// sends is API-valid.
	before := len(out.String())
	eng.HandlePrompt("again")
	eng.Wait()

	turn2 := frameTypes(t, out.String()[before:])
	for _, ty := range turn2 {
		if ty == "agent_error" {
			t.Fatalf("the prompt after the abort failed: %v\nlogs:\n%s", turn2, logs.String())
		}
	}
	assertFrameTypes(t, out.String()[before:], []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"message_start", "message_update", "message_end", // the scripted end_turn the abort preserved
		"agent_end", "agent_settled"})
	if got := sender.callCount(); got != 2 {
		t.Fatalf("sender served %d requests in total, want 2 (turn 1's tool_use + turn 2's reply)", got)
	}
	// The invariant RepairOrphans exists to protect: the request carries a
	// tool_result for tu_1, so the real API would accept it.
	assertToolResultFollowsToolUse(t, sender.lastParams(t).Messages, "tu_1")
}

// countFrames counts occurrences of one frame type.
func countFrames(types []string, want string) int {
	n := 0
	for _, ty := range types {
		if ty == want {
			n++
		}
	}
	return n
}
