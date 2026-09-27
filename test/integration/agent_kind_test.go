package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeTurnsScript writes a --fake-turns ndjson file (pkg/fundi's hidden
// test seam, LoadFakeSender) with two scripted assistant turns:
//
//  1. A tool_use call to the real "bash" tool running
//     `touch <markerPath> && read -r _ < <fifoPath>`. The touch announces
//     "the tool is genuinely executing" - a happens-before edge the test
//     blocks on before sending the abort. The subsequent `read` then blocks
//     forever in open(2) on the FIFO, since the test never opens fifoPath
//     for writing: there is no wall-clock margin anywhere, the turn can end
//     ONLY via the abort tearing down the tool's process group.
//  2. A plain end_turn reply, consumed by a second prompt sent after the
//     abort to prove the same child process still works.
//
// Aborting mid-tool cancels the turn's context, which is what actually kills
// the blocked read (see pkg/fundi/tools/bash.go's Setpgid+cmd.Cancel
// wiring) - the turn never reaches a second LLM call, so the fake sender's
// second scripted message is left for the second prompt.
func fakeTurnsScript(t *testing.T, markerPath, fifoPath string) string {
	t.Helper()
	c := assert.NewAborting(t)

	command := fmt.Sprintf("touch %s && read -r _ < %s", shellQuote(markerPath), shellQuote(fifoPath))
	commandJSON, err := json.Marshal(command)
	c.NoError(err, "marshal scripted command")

	toolUseTurn := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":%s}}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`, commandJSON)
	const endTurn = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(toolUseTurn+"\n"+endTurn+"\n"), 0o600), "write fake-turns script")
	return path
}

// shellQuote wraps s in single quotes for embedding in a `bash -c` command
// string. Test-only temp-dir paths never contain single quotes; this panics
// loudly instead of silently producing a broken scripted command if that
// assumption is ever violated.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if r == '\'' {
			panic(fmt.Sprintf("shellQuote: unsupported single quote in %q", s))
		}
	}
	return "'" + s + "'"
}

// waitForMarker polls for path to exist, timing out after timeout. This is
// the happens-before synchronization edge that proves the scripted bash
// tool has genuinely started executing (it touched its marker file) before
// the test proceeds to send the abort - a bounded poll guarded by a
// deadline, not a sleep used for synchronization.
func waitForMarker(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat marker file %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout (%v) waiting for marker file %s to appear", timeout, path)
}

// turnStarted / turnSettled are the per-turn witnesses this test waits on:
// a durable agent_status event carrying "streaming" / "idle" for childID.
//
// Deliberately NOT a weaker proxy for the framed plane's inner pi events
// than they were: agent_start and agent_settled both feed the same state
// machine (pkg/child/state.go — agent_start → streaming, agent_settled →
// idle), whose transitions monitorChild DRAINS loss-free and forwards to
// handleStatusChange, the daemon's only agent_status producer. Status events
// were only unreliable when they were SAMPLED per bus frame; see the doc
// comment on monitorChild (cmd/rafikid/controller.go) for why draining
// replaced that.
func turnStarted(childID string) func(*rafikiv1.Event) bool {
	return agentStatusEvent(childID, "streaming")
}

func turnSettled(childID string) func(*rafikiv1.Event) bool {
	return agentStatusEvent(childID, "idle")
}

// assistantTextIn returns the assistant message text carried by an
// assistant_message event for childID, or "" for any other event. It is how
// this test proves WHICH scripted turn a prompt consumed — the durable
// assistant_message event is where publishAssistant puts it.
func assistantTextIn(childID string, ev *rafikiv1.Event) string {
	am := ev.GetAssistantMessage()
	if am == nil || ev.GetChildId() != childID {
		return ""
	}
	for _, b := range am.GetContent() {
		if t := b.GetText(); t != nil && t.GetText() != "" {
			return t.GetText()
		}
	}
	return ""
}

// assertTurnEndedCleanlyBetween fails unless the events buffered from index
// `from` onward carry no error event AND a turn_end whose raw stop reason is
// "end_turn". The stream is child-scoped, so every buffered event is this
// child's; both requirements are checked independently of each other. `to` is
// the settled-idle index the caller waited on — a snapshot, not a hard bound:
// the turn's own turn_end is appended to the event log AFTER the agent_end
// stdout frame that produces that idle status (Emitter.AgentEnd writes the pi
// frames first, then publishes), so under load the append can land after the
// test arrives here. The helper therefore WAITS (bounded, like every other
// wait in this test) for the end_turn turn_end instead of scanning a frozen
// window, and the error scan runs over the buffer as it grows. Waiting cannot
// hide a failure: a failed turn publishes its native error event in runTurn's
// error arm, strictly BEFORE AgentEnd publishes any turn_end, so by the time
// an end_turn turn_end is visible any error for the same turn is already in
// the buffer and fatal first.
//
// The error half is the like-for-like native replacement for the framed
// plane's assertNoErrorEventBetween, which failed if any agent_error frame for
// the child appeared in the window — the exact signature of the context-blind
// fake sender bug (the aborted turn had consumed the second scripted message,
// so the follow-up prompt failed with "scripted turns exhausted"). fundi
// publishes that failure on the durable plane now too: runTurn's error arm
// (pkg/fundi/engine.go) publishes a native ErrorEvent through the same
// pipeline the turn_end below rides, so a StreamEvents subscriber sees it.
//
// The end_turn requirement is an INDEPENDENT check, not the error check: a
// turn_end's stop reason is copied from the last assistant reply
// (publishAssistant sets Emitter.lastStop, publishTurnEnd copies it), and
// lastStop is neither updated by a failing turn nor reset between turns — so
// a turn that fails can still publish a turn_end reading end_turn (the stale
// previous reply's reason). Requiring a turn_end with raw stop reason
// "end_turn" pins that an assistant turn reached a model end_turn here;
// requiring no error event pins that the turn did not fail; requiring the
// "done" assistant text (the caller checks it separately, before this runs)
// pins that the reply was THIS script's second message. All three together
// are the property the framed plane asserted.
func assertTurnEndedCleanlyBetween(t *testing.T, es *eventStream, from, to int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		es.mu.Lock()
		for i := from; i < len(es.events); i++ {
			if ee := es.events[i].GetError(); ee != nil {
				es.mu.Unlock()
				t.Fatalf("error event at index %d (window starts at %d) — the turn errored (code %q): %s; buffered events:\n%s",
					i, from, ee.GetCode(), ee.GetMessage(), es.dumpLocked())
			}
			if te := es.events[i].GetTurnEnd(); te != nil && te.GetRawStopReason() == "end_turn" {
				found = true
			}
		}
		dump := es.dumpLocked()
		es.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn_end with raw stop reason end_turn after index %d within 5s "+
				"(settled-idle snapshot at %d); buffered events:\n%s", from, to, dump)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoRestartBetween scans es's already-buffered events in the half-open
// range [from, to) and fails the test if any of them is a child_spawned event
// for childID. This is the restart witness for
// TestIntegration_AgentKind_AbortPreservesProcess: whether a respawn is a
// real subprocess re-exec (pi, claude) or an in-process respawn (agent, no
// pid), activateLiveChild's Resume/RespawnChild path re-publishes a native
// child_spawned event for the SAME childID (cmd/rafikid/controller.go), so
// absence of that event between the abort and the following idle states "the
// child was not restarted" directly, instead of inferring it from PID
// identity (which is degenerate for the agent kind — see the KEYSTONE
// ASSERTION comment below).
func assertNoRestartBetween(t *testing.T, es *eventStream, from, to int, childID string) {
	t.Helper()
	es.mu.Lock()
	defer es.mu.Unlock()
	for i := from; i < to && i < len(es.events); i++ {
		if cs := es.events[i].GetChildSpawned(); cs != nil && cs.GetChildId() == childID {
			t.Fatalf("child was restarted: unexpected child_spawned event for childId=%s "+
				"between abort and idle: %s", childID, es.events[i])
		}
	}
}

// TestIntegration_AgentKind_AbortPreservesProcess is the Task 16 keystone
// test: it spawns a real `rafikid fundi` child (kind="fundi") against the
// real daemon binary under test (this is why it lives in the subprocess
// integration harness -- it exercises bootDaemon's real controller/store/
// subscriber wiring end-to-end, not a property of the agent kind itself; as
// of Task 5, the agent kind runs in-process inside rafikid on a shared pool
// rather than self-exec'ing via os.Executable(), and has no pid of its own),
// drives a prompt into a scripted tool_use turn that blocks on a FIFO read
// the test never satisfies, aborts mid-turn, and proves the abort landed
// in-band (no restart) by witnessing the child's lifecycle events rather
// than PID identity - the same forwarding path TestSend_PiAbortForwardedNatively
// proves in-process for kind="pi" (Controller.Send only intercepts abort for
// kind=="claude"; both "pi" and "fundi" fall through to ch.Send, forwarded to
// the child's stdin/inproc.Runner natively).
//
// Because the scripted tool cannot finish on its own (the FIFO is never
// written to), reaching "idle" is possible ONLY through the abort
// interrupting an in-flight turn - there is no wall-clock race to win or
// lose; a test that forgot to send the abort would time out at the "idle"
// wait instead of passing for the wrong reason.
func TestIntegration_AgentKind_AbortPreservesProcess(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	// The scripted turn calls the real `bash` tool, which after the executor
	// rule requires an enrolled executor. Boot the grant daemon (postgres) so
	// the child can be placed on one; a fundi child with no executor has no
	// bash to block on.
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)
	d := g.daemon
	g.enrollExecutor(t, map[string]string{"env": "home"})
	g.waitForLiveExecutors(t, 1)

	scriptDir := t.TempDir()
	markerPath := filepath.Join(scriptDir, "tool-started")
	fifoPath := filepath.Join(scriptDir, "block.fifo")
	c.NoError(syscall.Mkfifo(fifoPath, 0o600), "mkfifo %s", fifoPath)
	// Teardown verification: the abort's process-group kill (see bash.go's
	// Setpgid+cmd.Cancel) should have reaped the blocked reader before this
	// runs. A non-blocking O_WRONLY open on a FIFO with no reader fails with
	// ENXIO; if it instead succeeds, some process still has fifoPath open
	// for reading - an orphaned blocked tool survived the test.
	t.Cleanup(func() {
		f, err := os.OpenFile(fifoPath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			f.Close()
			t.Error("orphaned blocked tool process: fifo still has a reader after test teardown")
			return
		}
		c.ErrorIs(err, syscall.ENXIO, "unexpected error probing fifo for leftover readers")
	})

	scriptPath := fakeTurnsScript(t, markerPath, fifoPath)

	client := d.control(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sresp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind: protocol.KindFundi,
		Cwd:  t.TempDir(),
		// --model is required by `rafikid fundi` (parseAgentFlags) since the
		// provider/model redesign; --fake-turns replaces the sender, so the
		// value itself is inert here beyond being provider-qualified.
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		ExtraArgs:        []string{"--fake-turns", scriptPath},
	}))
	c.NoError(err, "spawn (agent kind) failed")
	childID := sresp.Msg.GetChildId()
	c.NotEq("", childID, "spawn returned empty childId")

	// sessionId must get sniffed from the agent's get_state bootstrap reply
	// (internal/child/sniff.go), same mechanism used for pi children.
	var before *rafikiv1.ChildSummary
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		before = getChild(t, client, childID)
		if before.GetSessionId() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.False(before == nil || before.GetSessionId() == "", "sessionId was never sniffed from the agent child")
	c.NotNil(before.Pid, "GetChild returned a nil PID for a live child")
	pidBefore := *before.Pid

	// The watermark read here is what keeps the child's replayed pre-open
	// events (the spawn's own child_spawned and statuses) from satisfying any
	// wait below: only events appended after the stream opened — this child's
	// first turn — carry a greater ordinal.
	watermark := eventWatermark(t, client, childID)
	es := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: watermark - 1}, watermark)

	// Prompt 1: the scripted tool_use turn - the agent calls
	// bash("touch <marker> && read -r _ < <fifo>"), which genuinely blocks
	// (forever, absent the abort) in a subprocess.
	pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
	if _, err := client.SendFrame(pctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"go"}`,
	})); err != nil {
		t.Fatalf("SendFrame (prompt 1) failed: %v", err)
	}
	pcancel()

	eventIdx := 0
	_, eventIdx = es.waitEventAfter(t, eventIdx, turnStarted(childID), 5*time.Second)

	// Deterministic happens-before edge: block until the scripted tool has
	// actually touched its marker file, proving it is genuinely executing
	// (and about to block forever on the FIFO read) before the abort is
	// sent. This replaces wall-clock margin entirely - the tool cannot
	// finish on its own, so there is no race to win.
	waitForMarker(t, markerPath, 5*time.Second)

	// Snapshot the event cursor immediately before the abort is sent, so the
	// restart witness below can scan exactly the window between the abort and
	// the turn settling.
	preAbortIdx := eventIdx

	actx, acancel := context.WithTimeout(context.Background(), 15*time.Second)
	if _, err := client.SendFrame(actx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"abort"}`,
	})); err != nil {
		t.Fatalf("SendFrame (abort) failed: %v", err)
	}
	acancel()

	// The turn must settle - agent_settled is the child's own "this turn is
	// over" frame, emitted by the engine's AgentEnd after runTurn's abort arm
	// has run RepairOrphans. The tool cannot finish on its own (nothing ever
	// opens the FIFO for writing), so reaching this frame at all is only
	// possible through the abort.
	var settledIdx int
	_, settledIdx = es.waitEventAfter(t, eventIdx, turnSettled(childID), 10*time.Second)
	eventIdx = settledIdx

	// KEYSTONE ASSERTION: the abort must NOT have restarted the child.
	//
	// In-process children (kind="fundi") have no pid: Runner.PID() returns 0
	// (Task 3), so ChildSummary.Pid is a non-nil pointer to 0 for the entire
	// life of the child. That made the old `*after.Pid != pidBefore` check
	// compare 0 != 0, which can never fail -- the agent kind ended up with no
	// restart guard at all. Witness the restart directly instead: whether a
	// respawn is a real subprocess re-exec (pi, claude) or an in-process
	// respawn (agent), activateLiveChild's Resume/RespawnChild path re-emits
	// the spawn for the SAME childID to per-child subscribers (see
	// cmd/rafikid/controller.go), so its absence between the abort and the turn
	// settling states "not restarted" without relying on pid identity.
	assertNoRestartBetween(t, es, preAbortIdx, settledIdx, childID)

	after := getChild(t, client, childID)
	c.NotEq(string(protocol.StatusExited), after.GetStatus(), "child exited after abort; expected it to remain alive (in-band abort, no restart)")
	c.NotNil(after.Pid, "GetChild returned a nil PID for a live child after abort")
	// Secondary check: kept for kinds that still have a real pid (pi,
	// claude). Gated on pidBefore != 0 so it doesn't silently pass for the
	// agent kind, whose pid is always 0 -- see the KEYSTONE ASSERTION above,
	// which is what actually guards the agent kind now.
	if pidBefore != 0 && after.GetPid() != pidBefore {
		t.Fatalf("PID changed across abort: before=%d after=%d (abort must NOT restart the process)", pidBefore, after.GetPid())
	}

	// Prompt 2: prove the SAME process still works after the abort - this
	// consumes the fake-turns script's second (plain end_turn) message, which
	// the aborted turn must NOT have eaten.
	p2ctx, p2cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if _, err := client.SendFrame(p2ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"anything"}`,
	})); err != nil {
		t.Fatalf("SendFrame (prompt 2) failed: %v", err)
	}
	p2cancel()

	prePrompt2Idx := eventIdx
	_, eventIdx = es.waitEventAfter(t, eventIdx, turnStarted(childID), 5*time.Second)
	// The scripted second turn's assistant text is "done" (see
	// fakeTurnsScript). Requiring it, rather than just "a turn happened", is
	// what proves the aborted turn left the script alone: with a fake sender
	// that ignores its context, the post-abort iteration consumes this very
	// message and prompt 2 gets "scripted turns exhausted" instead.
	doneFrame, eventIdx := es.waitEventAfter(t, eventIdx, func(ev *rafikiv1.Event) bool {
		return assistantTextIn(childID, ev) == "done"
	}, 5*time.Second)
	c.NotNil(doneFrame, "no assistant reply for prompt 2")
	_, settled2Idx := es.waitEventAfter(t, eventIdx, turnSettled(childID), 5*time.Second)
	assertTurnEndedCleanlyBetween(t, es, prePrompt2Idx, settled2Idx)

	// Final PID check: still the same process throughout the second prompt.
	// Gated on pidBefore != 0 for the same reason as the KEYSTONE ASSERTION
	// above -- the agent kind's pid is always 0, so this only guards pi and
	// claude.
	final := getChild(t, client, childID)
	c.NotNil(final.Pid, "GetChild returned a nil PID for a live child after second prompt")
	if pidBefore != 0 && final.GetPid() != pidBefore {
		t.Fatalf("PID changed after second prompt: want %d, got %d", pidBefore, final.GetPid())
	}
}
