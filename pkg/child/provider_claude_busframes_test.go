package child

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// collectBusFrames replays a golden claude transcript through a single per-child
// claudeProvider (via Fresh) and returns the flat ordered list of pi event
// frames published on the bus, decoded to maps for inspection.
func collectBusFrames(t *testing.T, fixture string) []map[string]any {
	t.Helper()
	f, err := os.Open("testdata/claude/" + fixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	prov := (ClaudeProvider{}).Fresh()
	var frames []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	ts := int64(1000)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" {
			continue
		}
		for _, raw := range prov.BusFrames([]byte(ln), ts) {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("bus frame not valid JSON: %v (%s)", err, raw)
			}
			frames = append(frames, m)
		}
		ts++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return frames
}

func types(frames []map[string]any) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i], _ = f["type"].(string)
	}
	return out
}

// TestClaudeBusFrames_TextTurn asserts the emitted pi-event sequence for a plain
// text turn (no tool use): agent_start, message_start, message_update,
// message_end, agent_end. The init line emits nothing on the bus.
func TestClaudeBusFrames_TextTurn(t *testing.T) {
	frames := collectBusFrames(t, "startup_and_turn.jsonl")
	got := types(frames)

	// The fixture has two assistant lines (thinking, then text). Each assistant
	// line produces message_start/update/end; agent_start fires once at the turn
	// boundary; agent_end at the result.
	want := []string{
		"agent_start",
		"message_start", "message_update", "message_end",
		"message_start", "message_update", "message_end",
		"agent_end", "agent_settled",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("text turn sequence:\n got=%v\nwant=%v", got, want)
	}

	// agent_end must carry the full accumulated messages[] (2 assistant messages
	// — no user message in this fixture since the turn is unsolicited startup).
	end := frames[len(frames)-2]
	msgs, ok := end["messages"].([]any)
	if !ok {
		t.Fatalf("agent_end.messages not an array: %v", end["messages"])
	}
	if len(msgs) != 2 {
		t.Fatalf("agent_end.messages len = %d, want 2", len(msgs))
	}
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] != "assistant" {
			t.Fatalf("expected assistant messages, got role=%v", m["role"])
		}
	}
	if end["willRetry"] != false {
		t.Fatalf("agent_end.willRetry = %v, want false", end["willRetry"])
	}

	// The startup fixture's first assistant frame is thinking-only with EMPTY
	// thinking text (Fable 5 display:omitted), which is skipped — so NO thinking
	// content block is ever emitted (emitting {type:"thinking"} with no field
	// crashes pi's TUI). The "pong" reply maps to a pi text block.
	sawPong := false
	for _, fr := range frames {
		msg, ok := fr["message"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, c := range content {
			blk := c.(map[string]any)
			if blk["type"] == "thinking" {
				t.Fatalf("emitted a thinking block (the only claude one was empty, must be skipped): %v", blk)
			}
			if blk["type"] == "text" && blk["text"] == "pong" {
				sawPong = true
			}
		}
	}
	if !sawPong {
		t.Fatalf("expected a pi text block with \"pong\"")
	}
}

// TestClaudeBusFrames_ToolTurn asserts the tool-turn sequence includes
// tool_execution_start (from the assistant tool_use block) and
// tool_execution_end (from the user tool_result block), the toolCall content
// block maps to pi vocabulary (toolCall/arguments, not tool_use/input), and the
// final agent_end.messages contains the assistant + toolResult messages with a
// mapped toolResult.
func TestClaudeBusFrames_ToolTurn(t *testing.T) {
	frames := collectBusFrames(t, "turn_with_tool.jsonl")
	got := types(frames)

	// Fixture: assistant(tool_use) → rate_limit(noop) → user(tool_result) →
	// assistant(text) → result.
	want := []string{
		"agent_start",
		"message_start", "message_update", "tool_execution_start", "message_end",
		"tool_execution_end",
		"message_start", "message_update", "message_end",
		"agent_end", "agent_settled",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool turn sequence:\n got=%v\nwant=%v", got, want)
	}

	// The assistant's tool_use block must be mapped to a pi toolCall block:
	// type=toolCall, arguments (NOT input), id/name preserved.
	upd := frames[2] // first message_update (the tool_use assistant message)
	msg := upd["message"].(map[string]any)
	content := msg["content"].([]any)
	tc := content[0].(map[string]any)
	if tc["type"] != "toolCall" {
		t.Fatalf("content block type = %v, want toolCall", tc["type"])
	}
	if tc["name"] != "Bash" || tc["id"] != "toolu_018XnHaLcfeVC7UT82WBjdz6" {
		t.Fatalf("toolCall id/name wrong: %v", tc)
	}
	if _, ok := tc["input"]; ok {
		t.Fatalf("toolCall must use arguments not input: %v", tc)
	}
	args, ok := tc["arguments"].(map[string]any)
	if !ok || args["command"] != "ls" {
		t.Fatalf("toolCall arguments wrong: %v", tc["arguments"])
	}

	// The assistant message stopReason should be toolUse when it carries a tool
	// call.
	if msg["stopReason"] != "toolUse" {
		t.Fatalf("tool_use assistant stopReason = %v, want toolUse", msg["stopReason"])
	}

	// tool_execution_start carries toolCallId/toolName/args.
	tsFrame := frames[3]
	if tsFrame["toolCallId"] != "toolu_018XnHaLcfeVC7UT82WBjdz6" || tsFrame["toolName"] != "Bash" {
		t.Fatalf("tool_execution_start wrong: %v", tsFrame)
	}

	// tool_execution_end carries result + isError=false.
	teFrame := frames[5]
	if teFrame["type"] != "tool_execution_end" {
		t.Fatalf("frame[5] = %v, want tool_execution_end", teFrame["type"])
	}
	if teFrame["isError"] != false {
		t.Fatalf("tool_execution_end isError = %v, want false", teFrame["isError"])
	}
	// result must be pi's {content:[...]} shape, not a bare string — pi's TUI
	// (ToolExecutionComponent.updateResult) reads a tool call's output
	// exclusively from result.content and silently discards anything else.
	teResult, ok := teFrame["result"].(map[string]any)
	if !ok {
		t.Fatalf("tool_execution_end result is not an object: %v", teFrame["result"])
	}
	teContent, ok := teResult["content"].([]any)
	if !ok || len(teContent) != 1 {
		t.Fatalf("tool_execution_end result.content not a 1-element array: %v", teResult["content"])
	}
	teText, _ := teContent[0].(map[string]any)["text"].(string)
	if !strings.Contains(teText, "go.mod") {
		t.Fatalf("tool_execution_end result missing tool output: %v", teText)
	}

	// agent_end.messages: assistant(tool_use) + toolResult + assistant(text) = 3.
	end := frames[len(frames)-2]
	msgs := end["messages"].([]any)
	roles := make([]string, len(msgs))
	for i, raw := range msgs {
		roles[i] = raw.(map[string]any)["role"].(string)
	}
	wantRoles := []string{"assistant", "toolResult", "assistant"}
	if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("agent_end.messages roles = %v, want %v", roles, wantRoles)
	}

	// The toolResult message must be pi-shaped: toolCallId/toolName, content as a
	// text-block array, isError.
	tr := msgs[1].(map[string]any)
	if tr["toolCallId"] != "toolu_018XnHaLcfeVC7UT82WBjdz6" || tr["toolName"] != "Bash" {
		t.Fatalf("toolResult message toolCallId/toolName wrong: %v", tr)
	}
	trContent, ok := tr["content"].([]any)
	if !ok || len(trContent) != 1 {
		t.Fatalf("toolResult content not a 1-element array: %v", tr["content"])
	}
	if trContent[0].(map[string]any)["type"] != "text" {
		t.Fatalf("toolResult content block not text: %v", trContent[0])
	}
	if tr["isError"] != false {
		t.Fatalf("toolResult isError = %v, want false", tr["isError"])
	}
}

// TestClaudeBusFrames_StateIsolated asserts two children (two Fresh instances)
// do not share translation state: a tool turn on one must not leak its pending
// tool calls or accumulated messages into the other.
func TestClaudeBusFrames_StateIsolated(t *testing.T) {
	a := (ClaudeProvider{}).Fresh()
	b := (ClaudeProvider{}).Fresh()

	init := []byte(`{"type":"system","subtype":"init","session_id":"s","model":"m"}`)
	a.BusFrames(init, 1)
	b.BusFrames(init, 1)

	// Drive a full turn on a.
	a.BusFrames([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`), 2)
	aEnd := a.BusFrames([]byte(`{"type":"result","subtype":"success"}`), 3)
	if len(aEnd) != 2 {
		t.Fatalf("a result should emit agent_end + agent_settled, got %d", len(aEnd))
	}

	// b has seen no assistant message; its agent_end must carry zero messages.
	bEnd := b.BusFrames([]byte(`{"type":"result","subtype":"success"}`), 4)
	if len(bEnd) != 2 {
		t.Fatalf("b result should emit agent_end + agent_settled, got %d", len(bEnd))
	}
	var m map[string]any
	_ = json.Unmarshal(bEnd[0], &m)
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Fatalf("b agent_end.messages leaked from a: %v", msgs)
	}
}

// TestClaudeBusFrames_InitEmitsNothing asserts the system/init line produces no
// bus frames (it only sets model/sessionId state, observed via Parse).
func TestClaudeBusFrames_InitEmitsNothing(t *testing.T) {
	prov := (ClaudeProvider{}).Fresh()
	frames := prov.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"s","model":"claude-x"}`), 1)
	if len(frames) != 0 {
		t.Fatalf("init should emit no bus frames, got %d", len(frames))
	}
	// hook lines and rate_limit_event also emit nothing.
	if f := prov.BusFrames([]byte(`{"type":"system","subtype":"hook_started"}`), 2); len(f) != 0 {
		t.Fatalf("hook line should emit nothing, got %v", f)
	}
	if f := prov.BusFrames([]byte(`{"type":"rate_limit_event"}`), 3); len(f) != 0 {
		t.Fatalf("rate_limit_event should emit nothing, got %v", f)
	}
}
