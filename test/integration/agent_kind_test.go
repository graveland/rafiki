package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/protocol"
)

// fakeTurnsScript writes a --fake-turns ndjson file (internal/agent's hidden
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
// the blocked read (see internal/agent/tools/bash.go's Setpgid+cmd.Cancel
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
// this lets a caller require a *new* occurrence of a status that has already
// fired once before - status transitions like "streaming"/"idle" repeat
// across multiple prompts on the same child, so plain "first match anywhere
// in history" semantics would let a later wait succeed instantly on a stale
// frame from an earlier round.
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
	t.Fatalf("timeout (%v) waiting for event after index %d", timeout, from)
	return nil, from
}

func childStatusPredicate(childID, status string) func(json.RawMessage) bool {
	return func(f json.RawMessage) bool {
		var ev struct {
			Type    string `json:"type"`
			ChildID string `json:"childId"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(f, &ev) != nil {
			return false
		}
		return ev.Type == protocol.TypeCtrlChildStatus && ev.ChildID == childID && ev.Status == status
	}
}

// TestIntegration_AgentKind_AbortPreservesProcess is the Task 16 keystone
// test: it spawns a real `fundi agent` child (kind="agent", self-exec via
// os.Executable() - this is why it lives in the subprocess harness rather
// than cmd/fundi's in-process tests, which run inside a go-test binary whose
// own os.Executable() would re-exec the test binary, not cmd/fundi's real
// main()), drives a prompt into a scripted tool_use turn that blocks on a
// FIFO read the test never satisfies, aborts mid-turn, and proves the abort
// landed in-band (no process restart) via PID identity - the same
// forwarding path TestSend_PiAbortForwardedNatively proves in-process for
// kind="pi" (Controller.Send only intercepts abort for kind=="claude"; both
// "pi" and "agent" fall through to ch.Send, forwarded to the child's stdin
// natively).
//
// Because the scripted tool cannot finish on its own (the FIFO is never
// written to), reaching "idle" is possible ONLY through the abort
// interrupting an in-flight turn - there is no wall-clock race to win or
// lose; a test that forgot to send the abort would time out at the "idle"
// wait instead of passing for the wrong reason.
func TestIntegration_AgentKind_AbortPreservesProcess(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

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
		Kind: "agent",
		Cwd:  t.TempDir(),
		// --model is required by `fundi agent` (parseAgentFlags) since the
		// provider/model redesign; --fake-turns replaces the sender, so the
		// value itself is inert here beyond being provider-qualified.
		Model:     "anthropic/claude-x",
		ExtraArgs: []string{"--fake-turns", scriptPath},
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
	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childStatusPredicate(childID, string(protocol.StatusStreaming)), 5*time.Second)

	// Deterministic happens-before edge: block until the scripted tool has
	// actually touched its marker file, proving it is genuinely executing
	// (and about to block forever on the FIFO read) before the abort is
	// sent. This replaces wall-clock margin entirely - the tool cannot
	// finish on its own, so there is no race to win.
	waitForMarker(t, markerPath, 5*time.Second)

	abortJSON := fmt.Sprintf(`{"type":"ctrl_send","id":"a1","childId":%q,"frame":{"type":"abort"}}`, childID)
	raw = d.request(t, abortJSON)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (abort) failed: %+v", r.Error)
	}

	// The turn must settle back to idle - RepairOrphans cleans up the
	// dangling tool_use without consuming a second scripted LLM turn.
	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childStatusPredicate(childID, string(protocol.StatusIdle)), 10*time.Second)

	// KEYSTONE ASSERTION: the abort must NOT have restarted the process.
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
	if *after.PID != pidBefore {
		t.Fatalf("PID changed across abort: before=%d after=%d (abort must NOT restart the process)", pidBefore, *after.PID)
	}

	// Prompt 2: prove the SAME process still works after the abort - this
	// consumes the fake-turns script's second (plain end_turn) message.
	sendJSON2 := fmt.Sprintf(`{"type":"ctrl_send","id":"p2","childId":%q,"frame":{"type":"prompt","message":"anything"}}`, childID)
	raw = d.request(t, sendJSON2)
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_send (prompt 2) failed: %+v", r.Error)
	}

	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childStatusPredicate(childID, string(protocol.StatusStreaming)), 5*time.Second)
	_, eventIdx = waitForEventAfter(t, sc, eventIdx, childStatusPredicate(childID, string(protocol.StatusIdle)), 5*time.Second)

	// Final PID check: still the same process throughout the second prompt.
	raw = d.request(t, getJSON)
	mustUnmarshal(t, raw, &r)
	var final protocol.ChildSummary
	mustUnmarshal(t, r.Data, &final)
	if final.PID == nil || *final.PID != pidBefore {
		t.Fatalf("PID changed after second prompt: want %d, got %v", pidBefore, final.PID)
	}
}
