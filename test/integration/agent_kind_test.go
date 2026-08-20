package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
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

	command := fmt.Sprintf("touch %s && read -r _ < %s", shellQuote(markerPath), shellQuote(fifoPath))
	commandJSON, err := json.Marshal(command)
	if err != nil {
		t.Fatalf("marshal scripted command: %v", err)
	}

	toolUseTurn := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":%s}}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`, commandJSON)
	const endTurn = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	if err := os.WriteFile(path, []byte(toolUseTurn+"\n"+endTurn+"\n"), 0o600); err != nil {
		t.Fatalf("write fake-turns script: %v", err)
	}
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

// waitForEventAfter polls sc's event buffer starting at index from until it
// finds a frame matching predicate, returning that frame and the index just
// past it. Unlike subConn.waitForEvent (which always scans from the start),
// this lets a caller require a *new* occurrence of something that has already
// fired once before - agent_start/agent_settled repeat across every prompt on
// the same child, so plain "first match anywhere in history" semantics would
// let a later wait succeed instantly on a stale frame from an earlier round.
//
// On timeout it dumps every buffered frame. A wait on a stream of frames that
// says only "I didn't find it" leaves the reader guessing about which of
// "never emitted", "emitted before `from`", or "emitted in a different shape"
// happened, and all three have been live possibilities here.
func waitForEventAfter(t *testing.T, sc *subConn, from int, predicate func(json.RawMessage) bool, timeout time.Duration) (json.RawMessage, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sc.mu.Lock()
		for i := from; i < len(sc.events); i++ {
			if predicate(sc.events[i]) {
				f := sc.events[i]
				sc.mu.Unlock()
				return f, i + 1
			}
		}
		sc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	sc.mu.Lock()
	dump := make([]string, 0, len(sc.events))
	for i, e := range sc.events {
		dump = append(dump, fmt.Sprintf("  [%d] %s", i, e))
	}
	sc.mu.Unlock()
	t.Fatalf("timeout (%v) waiting for event after index %d; buffered events:\n%s", timeout, from, strings.Join(dump, "\n"))
	return nil, from
}

// childEventPredicate matches a ctrl_event envelope for childID carrying an
// inner pi event of type eventType.
//
// Deliberately NOT a ctrl_child_status predicate, which is what this test used
// to wait on. Status events are not a reliable witness of a turn: the daemon
// DERIVES them by sampling ch.Status() once per bus frame monitorChild
// receives (cmd/rafikid/controller.go), while readStdout updates the state
// machine as it races ahead. A turn short enough to emit all its frames before
// monitorChild's goroutine is next scheduled therefore completes its entire
// streaming -> idle round trip between two samples and produces NO status
// event at all. Measured, not theorised: the second prompt here (a scripted
// reply with no tool call, so it finishes in about a millisecond) emitted zero
// status frames in 5-7 runs out of 10, which is precisely why this test was
// red half the time.
//
// The pi events themselves are lossless — every frame readStdout reads is
// published to the bus — so they are what a turn should be witnessed by. See
// docs/plans/2026-07-30-phase1a-followups.md for the daemon-side bug.
func childEventPredicate(childID, eventType string) func(json.RawMessage) bool {
	return func(f json.RawMessage) bool {
		var ev struct {
			Type    string `json:"type"`
			ChildID string `json:"childId"`
			Event   struct {
				Type string `json:"type"`
			} `json:"event"`
		}
		if json.Unmarshal(f, &ev) != nil {
			return false
		}
		return ev.Type == protocol.TypeCtrlEvent && ev.ChildID == childID && ev.Event.Type == eventType
	}
}

// assistantTextIn returns the assistant message text carried by a
// message_end ctrl_event for childID, or "" for any other frame. It is how
// this test proves WHICH scripted turn a prompt consumed.
func assistantTextIn(childID string, f json.RawMessage) string {
	var ev struct {
		Type    string `json:"type"`
		ChildID string `json:"childId"`
		Event   struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"event"`
	}
	if json.Unmarshal(f, &ev) != nil {
		return ""
	}
	if ev.Type != protocol.TypeCtrlEvent || ev.ChildID != childID {
		return ""
	}
	if ev.Event.Type != "message_end" || ev.Event.Message.Role != "assistant" {
		return ""
	}
	for _, b := range ev.Event.Message.Content {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// assertNoErrorEventBetween fails if any agent_error frame for childID appears
// in sc's buffered range [from, to). An agent_error in the second prompt's
// window is the exact signature of the context-blind fake sender bug: the
// aborted turn had consumed the second scripted message, so the follow-up
// prompt failed with "scripted turns exhausted".
func assertNoErrorEventBetween(t *testing.T, sc *subConn, from, to int, childID string) {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i := from; i < to && i < len(sc.events); i++ {
		if childEventPredicate(childID, "agent_error")(sc.events[i]) {
			t.Fatalf("agent_error in window [%d,%d): %s", from, to, sc.events[i])
		}
	}
}

// assertNoRestartBetween scans sc's already-buffered events in the half-open
// range [from, to) and fails the test if any of them is a ctrl_child_spawned
// frame for childID. This is the restart witness for
// TestIntegration_AgentKind_AbortPreservesProcess: whether a respawn is a
// real subprocess re-exec (pi, claude) or an in-process respawn (agent, no
// pid), activateLiveChild's Resume/RespawnChild path re-emits
// ctrl_child_spawned for the SAME childID to per-child subscribers
// (cmd/rafikid/controller.go), so absence of that frame between the abort and
// the following idle states "the child was not restarted" directly, instead
// of inferring it from PID identity (which is degenerate for the agent kind
// -- see the KEYSTONE ASSERTION comment below).
func assertNoRestartBetween(t *testing.T, sc *subConn, from, to int, childID string) {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i := from; i < to && i < len(sc.events); i++ {
		var ev struct {
			Type    string `json:"type"`
			ChildID string `json:"childId"`
		}
		if json.Unmarshal(sc.events[i], &ev) != nil {
			continue
		}
		if ev.Type == protocol.TypeCtrlChildSpawned && ev.ChildID == childID {
			t.Fatalf("child was restarted: unexpected %s frame for childId=%s between abort and idle: %s",
				protocol.TypeCtrlChildSpawned, childID, sc.events[i])
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
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", fifoPath, err)
	}
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
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("unexpected error probing fifo for leftover readers: %v", err)
		}
	})

	scriptPath := fakeTurnsScript(t, markerPath, fifoPath)

	spawnReq := protocol.SpawnRequest{
		Type: "ctrl_spawn",
		ID:   "spawn1",
		Kind: protocol.KindFundi,
		Cwd:  t.TempDir(),
		// --model is required by `rafikid fundi` (parseAgentFlags) since the
		// provider/model redesign; --fake-turns replaces the sender, so the
		// value itself is inert here beyond being provider-qualified.
		Model:            "anthropic/claude-x",
		ExecutorSelector: "env=home",
		ExtraArgs:        []string{"--fake-turns", scriptPath},
	}
	spawnFrame, err := json.Marshal(spawnReq)
	if err != nil {
		t.Fatalf("marshal spawn request: %v", err)
	}

	raw := d.request(t, string(spawnFrame))
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_spawn (agent kind) failed: %+v", r.Error)
	}
	var spawnData protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &spawnData)
	childID := spawnData.ChildID
	if childID == "" {
		t.Fatal("spawn returned empty childId")
	}

	getJSON := fmt.Sprintf(`{"type":"ctrl_get","id":"g1","childId":%q}`, childID)

	// sessionId must get sniffed from the agent's get_state bootstrap reply
	// (internal/child/sniff.go), same mechanism used for pi children.
	var before protocol.ChildSummary
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw = d.request(t, getJSON)
		mustUnmarshal(t, raw, &r)
		if !r.Success {
			t.Fatalf("ctrl_get failed: %+v", r.Error)
		}
		mustUnmarshal(t, r.Data, &before)
		if before.SessionID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if before.SessionID == "" {
		t.Fatal("sessionId was never sniffed from the agent child")
	}
	if before.PID == nil {
		t.Fatal("ctrl_get returned a nil PID for a live child")
	}
	pidBefore := *before.PID

	sc := d.dial(t)
	sc.send(fmt.Sprintf(`{"type":"ctrl_subscribe","id":"sub1","childId":%q}`, childID))
	subResp := sc.nextResponse(t, 5*time.Second)
	if !subResp.Success {
		t.Fatalf("ctrl_subscribe failed: %+v", subResp.Error)
	}

	// Prompt 1: the scripted tool_use turn - the agent calls
	// bash("touch <marker> && read -r _ < <fifo>"), which genuinely blocks
	// (forever, absent the abort) in a subprocess.
	sendJSON := fmt.Sprintf(`{"type":"ctrl_send","id":"p1","childId":%q,"frame":{"type":"prompt","message":"go"}}`, childID)
	raw = d.request(t, sendJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (prompt 1) failed: %+v", r.Error)
	}

	eventIdx := 0
	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childEventPredicate(childID, "agent_start"), 5*time.Second)

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

	abortJSON := fmt.Sprintf(`{"type":"ctrl_send","id":"a1","childId":%q,"frame":{"type":"abort"}}`, childID)
	raw = d.request(t, abortJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (abort) failed: %+v", r.Error)
	}

	// The turn must settle - agent_settled is the child's own "this turn is
	// over" frame, emitted by the engine's AgentEnd after runTurn's abort arm
	// has run RepairOrphans. The tool cannot finish on its own (nothing ever
	// opens the FIFO for writing), so reaching this frame at all is only
	// possible through the abort.
	var settledIdx int
	_, settledIdx = waitForEventAfter(t, sc, eventIdx, childEventPredicate(childID, "agent_settled"), 10*time.Second)
	eventIdx = settledIdx

	// KEYSTONE ASSERTION: the abort must NOT have restarted the child.
	//
	// In-process children (kind="fundi") have no pid: Runner.PID() returns 0
	// (Task 3), so ChildSummary.PID is a non-nil pointer to 0 for the entire
	// life of the child. That made the old `*after.PID != pidBefore` check
	// compare 0 != 0, which can never fail -- the agent kind ended up with no
	// restart guard at all. Witness the restart directly instead: whether a
	// respawn is a real subprocess re-exec (pi, claude) or an in-process
	// respawn (agent), activateLiveChild's Resume/RespawnChild path re-emits
	// ctrl_child_spawned for the SAME childID to per-child subscribers (see
	// cmd/rafikid/controller.go), so its absence between the abort and the turn
	// settling states "not restarted" without relying on pid identity.
	assertNoRestartBetween(t, sc, preAbortIdx, settledIdx, childID)

	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_get after abort failed: %+v", r.Error)
	}
	var after protocol.ChildSummary
	mustUnmarshal(t, r.Data, &after)
	if after.Status == string(protocol.StatusExited) {
		t.Fatal("child exited after abort; expected it to remain alive (in-band abort, no restart)")
	}
	if after.PID == nil {
		t.Fatal("ctrl_get returned a nil PID for a live child after abort")
	}
	// Secondary check: kept for kinds that still have a real pid (pi,
	// claude). Gated on pidBefore != 0 so it doesn't silently pass for the
	// agent kind, whose pid is always 0 -- see the KEYSTONE ASSERTION above,
	// which is what actually guards the agent kind now.
	if pidBefore != 0 && *after.PID != pidBefore {
		t.Fatalf("PID changed across abort: before=%d after=%d (abort must NOT restart the process)", pidBefore, *after.PID)
	}

	// Prompt 2: prove the SAME process still works after the abort - this
	// consumes the fake-turns script's second (plain end_turn) message, which
	// the aborted turn must NOT have eaten.
	sendJSON2 := fmt.Sprintf(`{"type":"ctrl_send","id":"p2","childId":%q,"frame":{"type":"prompt","message":"anything"}}`, childID)
	raw = d.request(t, sendJSON2)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (prompt 2) failed: %+v", r.Error)
	}

	prePrompt2Idx := eventIdx
	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childEventPredicate(childID, "agent_start"), 5*time.Second)
	// The scripted second turn's assistant text is "done" (see
	// fakeTurnsScript). Requiring it, rather than just "a turn happened", is
	// what proves the aborted turn left the script alone: with a fake sender
	// that ignores its context, the post-abort iteration consumes this very
	// message and prompt 2 gets "scripted turns exhausted" instead.
	doneFrame, eventIdx := waitForEventAfter(t, sc, eventIdx, func(f json.RawMessage) bool {
		return assistantTextIn(childID, f) == "done"
	}, 5*time.Second)
	if doneFrame == nil {
		t.Fatal("no assistant reply for prompt 2")
	}
	_, settled2Idx := waitForEventAfter(t, sc, eventIdx, childEventPredicate(childID, "agent_settled"), 5*time.Second)
	assertNoErrorEventBetween(t, sc, prePrompt2Idx, settled2Idx, childID)

	// Final PID check: still the same process throughout the second prompt.
	// Gated on pidBefore != 0 for the same reason as the KEYSTONE ASSERTION
	// above -- the agent kind's pid is always 0, so this only guards pi and
	// claude.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var final protocol.ChildSummary
	mustUnmarshal(t, r.Data, &final)
	if final.PID == nil {
		t.Fatal("ctrl_get returned a nil PID for a live child after second prompt")
	}
	if pidBefore != 0 && *final.PID != pidBefore {
		t.Fatalf("PID changed after second prompt: want %d, got %v", pidBefore, *final.PID)
	}
}
