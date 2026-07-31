package child

import (
	"encoding/json"
	"testing"
)

// TestPiAssistantMessage_RequiredFields asserts that a marshalled AssistantMessage
// carries every field the pi AssistantMessage TS type marks non-optional
// (role, content, api, provider, model, usage, stopReason, timestamp) plus a
// fully-populated usage object. The attach layer's renderer reads role/content;
// the pi-ai type system requires the rest, so a child that omits them produces a
// frame the TUI may reject when it casts to AssistantMessage.
func TestPiAssistantMessage_RequiredFields(t *testing.T) {
	msg := PiAssistantMessage{
		Role: "assistant",
		Content: []PiContentBlock{
			PiTextBlock("hello"),
			PiToolCallBlock("t1", "Bash", map[string]any{"command": "ls"}),
		},
		API:        "anthropic-messages",
		Provider:   "anthropic",
		Model:      "claude-fable-5",
		StopReason: "toolUse",
		Timestamp:  1234,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"role", "content", "api", "provider", "model", "usage", "stopReason", "timestamp"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("assistant message missing required key %q: %s", k, b)
		}
	}
	if m["role"] != "assistant" {
		t.Fatalf("role = %v", m["role"])
	}
	if m["stopReason"] != "toolUse" {
		t.Fatalf("stopReason = %v", m["stopReason"])
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage not an object: %v", m["usage"])
	}
	for _, k := range []string{"input", "output", "cacheRead", "cacheWrite", "totalTokens", "cost"} {
		if _, ok := usage[k]; !ok {
			t.Fatalf("usage missing required key %q: %s", k, b)
		}
	}
	cost, ok := usage["cost"].(map[string]any)
	if !ok {
		t.Fatalf("usage.cost not an object: %v", usage["cost"])
	}
	for _, k := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
		if _, ok := cost[k]; !ok {
			t.Fatalf("usage.cost missing required key %q: %s", k, b)
		}
	}
}

// TestPiContentBlocks_TypeStrings asserts the content-block discriminator strings
// match the pi-ai TS unions exactly: text/thinking/toolCall (NOT tool_use), and
// the toolCall block uses `arguments` (NOT input).
func TestPiContentBlocks_TypeStrings(t *testing.T) {
	cases := []struct {
		block    PiContentBlock
		wantType string
		wantKey  string // a discriminating field that must be present
	}{
		{PiTextBlock("hi"), "text", "text"},
		{PiThinkingBlock("pondering"), "thinking", "thinking"},
		{PiToolCallBlock("t1", "Bash", map[string]any{"command": "ls"}), "toolCall", "arguments"},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.block)
		if err != nil {
			t.Fatalf("marshal %s: %v", c.wantType, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m["type"] != c.wantType {
			t.Fatalf("block type = %v, want %q (%s)", m["type"], c.wantType, b)
		}
		if _, ok := m[c.wantKey]; !ok {
			t.Fatalf("block %q missing key %q: %s", c.wantType, c.wantKey, b)
		}
	}

	// A toolCall must carry id/name/arguments and must NOT carry input.
	tc := PiToolCallBlock("toolu_1", "Bash", map[string]any{"command": "ls"})
	b, _ := json.Marshal(tc)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["id"] != "toolu_1" || m["name"] != "Bash" {
		t.Fatalf("toolCall id/name wrong: %s", b)
	}
	if _, bad := m["input"]; bad {
		t.Fatalf("toolCall must not use `input` (pi uses `arguments`): %s", b)
	}
}

// TestPiUserMessage_Shape asserts a UserMessage carries role/content/timestamp.
func TestPiUserMessage_Shape(t *testing.T) {
	u := PiUserMessage{Role: "user", Content: "hi there", Timestamp: 9}
	b, _ := json.Marshal(u)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"role", "content", "timestamp"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("user message missing %q: %s", k, b)
		}
	}
	if m["role"] != "user" || m["content"] != "hi there" {
		t.Fatalf("user message wrong: %s", b)
	}
}

// TestPiToolResultMessage_Shape asserts a ToolResultMessage carries the
// toolResult role, toolCallId/toolName, a content array of text blocks, isError,
// and timestamp — the shape pi-ai's ToolResultMessage type requires.
func TestPiToolResultMessage_Shape(t *testing.T) {
	tr := PiToolResultMessage{
		Role:       "toolResult",
		ToolCallID: "toolu_1",
		ToolName:   "Bash",
		Content:    []PiContentBlock{PiTextBlock("listing...")},
		IsError:    false,
		Timestamp:  11,
	}
	b, _ := json.Marshal(tr)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"role", "toolCallId", "toolName", "content", "isError", "timestamp"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("toolResult missing %q: %s", k, b)
		}
	}
	if m["role"] != "toolResult" || m["toolCallId"] != "toolu_1" {
		t.Fatalf("toolResult wrong: %s", b)
	}
	content, ok := m["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("toolResult content not a 1-element array: %s", b)
	}
}

// TestPiEvents_TypeDiscriminators asserts the emitted AgentSessionEvent JSON
// frames carry the exact `type` discriminators and required sibling fields.
func TestPiEvents_TypeDiscriminators(t *testing.T) {
	asst := PiAssistantMessage{Role: "assistant", Content: []PiContentBlock{PiTextBlock("hi")}, API: "anthropic-messages", Provider: "anthropic", Model: "m", StopReason: "stop", Timestamp: 1}

	mustType := func(t *testing.T, frame []byte, want string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(frame, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", want, err)
		}
		if m["type"] != want {
			t.Fatalf("type = %v, want %q: %s", m["type"], want, frame)
		}
		return m
	}

	mustType(t, mustMarshal(t, PiAgentStart()), "agent_start")

	mStart := mustType(t, mustMarshal(t, PiMessageStart(asst, "")), "message_start")
	if _, ok := mStart["message"]; !ok {
		t.Fatal("message_start missing message")
	}

	mUpd := mustType(t, mustMarshal(t, PiMessageUpdate(asst, "")), "message_update")
	if _, ok := mUpd["message"]; !ok {
		t.Fatal("message_update missing message")
	}
	if _, ok := mUpd["assistantMessageEvent"]; !ok {
		t.Fatal("message_update missing assistantMessageEvent")
	}

	mEnd := mustType(t, mustMarshal(t, PiMessageEnd(asst, "")), "message_end")
	if _, ok := mEnd["message"]; !ok {
		t.Fatal("message_end missing message")
	}

	ts := mustType(t, mustMarshal(t, PiToolExecutionStart("t1", "Bash", map[string]any{"command": "ls"}, "")), "tool_execution_start")
	if ts["toolCallId"] != "t1" || ts["toolName"] != "Bash" {
		t.Fatalf("tool_execution_start fields wrong: %v", ts)
	}
	if _, ok := ts["args"]; !ok {
		t.Fatal("tool_execution_start missing args")
	}

	te := mustType(t, mustMarshal(t, PiToolExecutionEnd("t1", "Bash", "ok", false, "")), "tool_execution_end")
	if te["toolCallId"] != "t1" || te["isError"] != false {
		t.Fatalf("tool_execution_end fields wrong: %v", te)
	}
	if _, ok := te["result"]; !ok {
		t.Fatal("tool_execution_end missing result")
	}

	ae := mustType(t, mustMarshal(t, PiAgentEnd([]json.RawMessage{mustMarshal(t, asst)}, nil)), "agent_end")
	msgs, ok := ae["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("agent_end messages not a 1-element array: %v", ae["messages"])
	}
	if ae["willRetry"] != false {
		t.Fatalf("agent_end willRetry = %v, want false", ae["willRetry"])
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
