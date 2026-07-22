package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

const sampleResp = `{
 "id":"msg_1","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"text","text":"on it"},
            {"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`

func TestAssistantTurnEmitsPiFrames(t *testing.T) {
	var resp anthropic.Message
	if err := json.Unmarshal([]byte(sampleResp), &resp); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic", "claude-x")
	em.AgentStart()
	em.UserMessage("go")
	em.AssistantTurn(&resp)
	em.ToolStart("tu_1", "bash", json.RawMessage(`{"command":"ls"}`))
	em.ToolEnd("tu_1", "bash", "file.txt", false)
	em.AgentEnd()

	var types []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		types = append(types, f.Type)
	}
	want := []string{"agent_start", "message_start", "message_end", // user echo
		"message_start", "message_update", "message_end", // assistant
		"tool_execution_start", "tool_execution_end",
		"agent_end", "agent_settled"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("frame sequence:\n got %v\nwant %v", types, want)
	}
	// spot-check mapping on the assistant message_end frame
	var me struct {
		Message struct {
			StopReason string           `json:"stopReason"`
			Content    []map[string]any `json:"content"`
			Usage      struct{ Input, Output int }
		} `json:"message"`
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if err := json.Unmarshal([]byte(lines[5]), &me); err != nil {
		t.Fatalf("unmarshal message_end frame: %v", err)
	}
	if me.Message.StopReason != "toolUse" {
		t.Fatalf("stopReason: %s", me.Message.StopReason)
	}
	if me.Message.Content[1]["type"] != "toolCall" {
		t.Fatalf("content[1]: %v", me.Message.Content[1])
	}
	if me.Message.Usage.Input != 10 || me.Message.Usage.Output != 5 {
		t.Fatalf("usage: %+v", me.Message.Usage)
	}
	// agent_end carries the 3 accumulated messages: user echo, assistant, toolResult
	var ae struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(lines[8]), &ae); err != nil {
		t.Fatalf("unmarshal agent_end frame: %v", err)
	}
	if len(ae.Messages) != 3 {
		t.Fatalf("agent_end messages: %d", len(ae.Messages))
	}
}

// TestMapAssistantMessageEmptyContentIsEmptyArray guards against a nil
// Content slice marshaling as JSON null: the pi TUI expects content to
// always be an array, even when a response yields no mappable blocks (e.g.
// only block types this mapper doesn't handle yet).
func TestMapAssistantMessageEmptyContentIsEmptyArray(t *testing.T) {
	const resp = `{
 "id":"msg_2","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(resp), &msg); err != nil {
		t.Fatal(err)
	}
	mapped := MapAssistantMessage(&msg, "anthropic")
	b, err := json.Marshal(mapped)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["content"]) != "[]" {
		t.Fatalf("content = %s, want []", raw["content"])
	}
	if mapped.StopReason != "stop" {
		t.Fatalf("stopReason = %q, want stop (default for end_turn)", mapped.StopReason)
	}
}
