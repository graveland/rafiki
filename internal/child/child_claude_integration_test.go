package child

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/protocol"
)

// writeFakeClaude writes a bash script that mimics claude's stream-json stdio:
// emit system/init immediately, then for every stdin line emit an assistant
// line followed by a result line. It also appends each received stdin line to
// $CAPTURE so the test can assert the outbound encoding.
func writeFakeClaude(t *testing.T, capturePath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	body := `#!/bin/bash
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-int","model":"claude-opus-4-8"}'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$CAPTURE"
  printf '%s\n' '{"type":"assistant","session_id":"sess-int","message":{"content":[{"type":"text","text":"ok"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"sess-int"}'
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	_ = capturePath
	return script
}

func TestClaudeChild_EndToEnd(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.txt")
	bin := writeFakeClaude(t, capture)

	cwd, _ := os.Getwd()
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_test",
		Cwd:      cwd,
		PiBinary: bin,
		Env:      []string{"CAPTURE=" + capture},
		Provider: ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	// Process-up readiness: claude is idle on spawn (ReadyOnSpawn), before init.
	select {
	case <-ch.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("child never became idle")
	}
	if st := ch.Status(); st != protocol.StatusIdle {
		t.Fatalf("status on spawn = %q, want idle", st)
	}
	// The fake emits system/init at startup; the session id is sniffed shortly
	// after — not necessarily by the instant Idle() closes (process-up readiness).
	sidDeadline := time.Now().Add(3 * time.Second)
	for ch.Metadata().SessionID == "" && time.Now().Before(sidDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ch.Metadata().SessionID; got != "sess-int" {
		t.Fatalf("session id = %q, want sess-int", got)
	}

	// Send a normalized prompt; the provider must encode it as a claude user
	// envelope, and the assistant→result sequence must return the child to idle.
	if err := ch.Send([]byte(`{"type":"prompt","message":"hi"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ch.Status() == protocol.StatusIdle {
			b, _ := os.ReadFile(capture)
			if strings.Contains(string(b), `"type":"user"`) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	b, _ := os.ReadFile(capture)
	line := strings.TrimSpace(string(b))
	if line == "" {
		t.Fatal("fake claude received no stdin frame")
	}
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &env); err != nil {
		t.Fatalf("captured frame not JSON: %v (%q)", err, line)
	}
	if env.Type != "user" || env.Message.Content != "hi" {
		t.Fatalf("outbound frame = %q, want a claude user envelope with content 'hi'", line)
	}
}

// writeFakeClaudeToolTurn writes a fake-claude that, on the first stdin line,
// emits a full tool turn: assistant(tool_use) → user(tool_result) →
// assistant(text) → result. This exercises the daemon's bus normalization end
// to end (tool_execution_start/end + a toolResult message in agent_end).
func writeFakeClaudeToolTurn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude_tool.sh")
	body := `#!/bin/bash
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-tool","model":"claude-fable-5"}'
read -r line
printf '%s\n' '{"type":"assistant","session_id":"sess-tool","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"tool_use","id":"toolu_X","name":"Bash","input":{"command":"ls"}}]}}'
printf '%s\n' '{"type":"user","session_id":"sess-tool","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"go.mod\ngo.sum","is_error":false}]}}'
printf '%s\n' '{"type":"assistant","session_id":"sess-tool","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"Done."}]}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"sess-tool","result":"Done."}'
sleep 30
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude tool: %v", err)
	}
	return script
}

// TestClaudeChild_BusCarriesNormalizedPiFrames is the Plan 4 correctness gate. It
// drives a fake-claude tool turn through the real Child loop and asserts:
//   - the BUS delivers the pi-vocabulary sequence (agent_start, message_start/
//     update/end, tool_execution_start/end, agent_end) — NOT raw claude
//     assistant/user/result frames.
//   - agent_end.messages contains the assistant(tool_use) + toolResult +
//     assistant(text) messages with mapped pi content blocks.
//   - the RING still holds the RAW claude frames (subagent_view raw fidelity).
func TestClaudeChild_BusCarriesNormalizedPiFrames(t *testing.T) {
	bin := writeFakeClaudeToolTurn(t)
	cwd, _ := os.Getwd()
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_bus",
		Cwd:      cwd,
		PiBinary: bin,
		Provider: ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	// Subscribe to the bus BEFORE prompting so we capture the whole sequence.
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	select {
	case <-ch.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("child never became idle from system/init")
	}

	if err := ch.Send([]byte(`{"type":"prompt","message":"list files"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Collect bus frames until we observe agent_end (the turn terminator).
	var busFrames []map[string]any
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case raw, ok := <-busCh:
			if !ok {
				break loop
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("bus frame not valid JSON: %v (%s)", err, raw)
			}
			busFrames = append(busFrames, m)
			if m["type"] == "agent_end" {
				break loop
			}
		case <-deadline:
			t.Fatalf("timed out waiting for agent_end; saw bus types %v", busTypes(busFrames))
		}
	}

	// The bus must carry the pi-vocabulary sequence, NOT raw claude frames.
	// The leading message_start/message_end is the synthesized user echo: claude
	// never echoes the prompt on stdout, so the daemon emits it when forwarding
	// the prompt (before the child responds), ahead of agent_start.
	gotTypes := busTypes(busFrames)
	want := []string{
		"message_start", "message_end",
		"agent_start",
		"message_start", "message_update", "tool_execution_start", "message_end",
		"tool_execution_end",
		"message_start", "message_update", "message_end",
		"agent_end",
	}
	if strings.Join(gotTypes, ",") != strings.Join(want, ",") {
		t.Fatalf("bus sequence:\n got=%v\nwant=%v", gotTypes, want)
	}

	// No raw claude frame types should ever appear on the bus.
	for _, ty := range gotTypes {
		switch ty {
		case "assistant", "user", "result", "system":
			t.Fatalf("bus carried a RAW claude frame type %q — bus must be pi-vocabulary only", ty)
		}
	}

	// agent_end.messages: user(echo) + assistant(tool_use) + toolResult + assistant(text).
	// The echoed user prompt is recorded first so a post-turn cache rebuild keeps it.
	end := busFrames[len(busFrames)-1]
	msgs, ok := end["messages"].([]any)
	if !ok {
		t.Fatalf("agent_end.messages not an array: %v", end["messages"])
	}
	roles := make([]string, len(msgs))
	for i, raw := range msgs {
		roles[i] = raw.(map[string]any)["role"].(string)
	}
	if strings.Join(roles, ",") != "user,assistant,toolResult,assistant" {
		t.Fatalf("agent_end.messages roles = %v, want [user assistant toolResult assistant]", roles)
	}
	// The first assistant message must carry a mapped pi toolCall block.
	a0 := msgs[1].(map[string]any)
	block0 := a0["content"].([]any)[0].(map[string]any)
	if block0["type"] != "toolCall" || block0["name"] != "Bash" {
		t.Fatalf("agent_end assistant content not a mapped toolCall: %v", block0)
	}
	if _, bad := block0["input"]; bad {
		t.Fatalf("toolCall must use arguments not input: %v", block0)
	}
	// The toolResult message must be pi-shaped.
	tr := msgs[2].(map[string]any)
	if tr["toolCallId"] != "toolu_X" || tr["toolName"] != "Bash" {
		t.Fatalf("toolResult message not paired: %v", tr)
	}

	// The RING must still hold the RAW claude frames (forensic fidelity).
	rawFrames := ch.RingSnapshot()
	sawRawAssistant, sawRawResult, sawRawToolUse := false, false, false
	for _, rf := range rawFrames {
		var m map[string]any
		if err := json.Unmarshal(rf, &m); err != nil {
			continue
		}
		switch m["type"] {
		case "assistant":
			sawRawAssistant = true
			if msg, ok := m["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if cb, ok := c.(map[string]any); ok && cb["type"] == "tool_use" {
							sawRawToolUse = true
						}
					}
				}
			}
		case "result":
			sawRawResult = true
		}
	}
	if !sawRawAssistant || !sawRawResult {
		t.Fatalf("ring lost raw claude frames: assistant=%v result=%v (ring=%d frames)", sawRawAssistant, sawRawResult, len(rawFrames))
	}
	if !sawRawToolUse {
		t.Fatal("ring must preserve the raw claude tool_use block (not the mapped toolCall)")
	}
}

// writeFakeClaudeSilentUntilInput mimics the REAL claude un-prompted behavior:
// it emits NOTHING on stdout until the first user message arrives (verified
// against the real binary: zero bytes with stdin open + no input). On the first
// stdin line it emits the deferred hook lifecycle + init + a one-line turn. This
// is the case that left a freshly-spawned, un-prompted child stuck in spawning
// (subagent_send, idle-gated, rejected forever).
func writeFakeClaudeSilentUntilInput(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude_silent.sh")
	body := `#!/bin/bash
# Silent until prompted — emit nothing until a line is read from stdin.
while IFS= read -r line; do
  printf '%s\n' '{"type":"system","subtype":"hook_started","session_id":"sess-real"}'
  printf '%s\n' '{"type":"system","subtype":"hook_response","session_id":"sess-real"}'
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-real","model":"claude-opus-4-8"}'
  printf '%s\n' '{"type":"assistant","session_id":"sess-real","message":{"content":[{"type":"text","text":"OK"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"sess-real"}'
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return script
}

// TestClaudeChild_IdleOnSpawnWhenSilent is the readiness golden test. It drives a
// fake-claude that reproduces the REAL un-prompted behavior (silent on stdout
// until prompted) and asserts:
//   - the child reaches idle from process-up readiness (ReadyOnSpawn), before any
//     prompt and with NO stdout at all (the bug: it stayed spawning until a steer);
//   - the session id is unknown until the first turn;
//   - a subsequent send is accepted and answered, returning the child to idle and
//     surfacing the session id — proving an un-prompted claude child is usable
//     without a steer workaround, and that resume metadata is still captured.
func TestClaudeChild_IdleOnSpawnWhenSilent(t *testing.T) {
	bin := writeFakeClaudeSilentUntilInput(t)
	cwd, _ := os.Getwd()
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_silent",
		Cwd:      cwd,
		PiBinary: bin,
		Provider: ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	// Core of the fix: a claude child emitting NOTHING on stdout must still reach
	// idle, from process-up readiness, before any prompt.
	select {
	case <-ch.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("silent claude child never became idle (stuck spawning)")
	}
	if st := ch.Status(); st != protocol.StatusIdle {
		t.Fatalf("status on spawn = %q, want idle", st)
	}
	if sid := ch.Metadata().SessionID; sid != "" {
		t.Fatalf("session id should be empty before the first turn, got %q", sid)
	}

	// Subscribe before sending so we capture the whole turn, then prove the send
	// is processed: the deferred init + assistant + result must drive a turn that
	// ends with agent_end, returns to idle, and surfaces the session id.
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	if err := ch.Send([]byte(`{"type":"prompt","message":"reply OK"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw, ok := <-busCh:
			if !ok {
				t.Fatal("bus closed before agent_end")
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("bus frame not JSON: %v (%s)", err, raw)
			}
			if m["type"] == "agent_end" {
				if st := ch.Status(); st != protocol.StatusIdle {
					t.Fatalf("status after turn = %q, want idle", st)
				}
				if sid := ch.Metadata().SessionID; sid != "sess-real" {
					t.Fatalf("session id after turn = %q, want sess-real", sid)
				}
				return
			}
		case <-deadline:
			t.Fatal("turn did not complete (no agent_end) after send to a silent un-prompted child")
		}
	}
}

func busTypes(frames []map[string]any) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i], _ = f["type"].(string)
	}
	return out
}
