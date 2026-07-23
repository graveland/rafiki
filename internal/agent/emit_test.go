package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// silenceSlog swaps the default slog logger for a discard handler for the
// duration of t, restoring it on cleanup. Used by tests that intentionally
// exercise a logged _raw-fallback path, so `go test -v` output stays clean.
func silenceSlog(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

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
	em := NewEmitter(fe, "anthropic")
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

// TestUserMessageAssignsUniqueID guards the pi consumer's message_end dedup
// contract: internal/child/pi_events.go documents that the consumer appends
// on message_end "deduping by id" (see claudeUserEcho's ID: fmt.Sprintf
// ("user-%d", ts) precedent). An always-empty ID would collide every user
// turn in the cache.
func TestUserMessageAssignsUniqueID(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic")
	em.UserMessage("first")
	// A real turn always separates two UserMessage calls by at least one LLM
	// round trip; sleep to guarantee distinct millisecond timestamps rather
	// than asserting uniqueness at zero elapsed time, which the ts-based
	// scheme (matching the claudeUserEcho precedent) was never meant to give.
	time.Sleep(2 * time.Millisecond)
	em.UserMessage("second")

	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		ids = append(ids, f.Message.ID)
	}
	// message_start + message_end for each of 2 UserMessage calls = 4 frames.
	if len(ids) != 4 {
		t.Fatalf("got %d frames, want 4: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id == "" {
			t.Fatalf("frame has empty message id: %v", ids)
		}
	}
	if ids[0] != ids[1] {
		t.Fatalf("message_start/message_end id mismatch for first UserMessage: %q vs %q", ids[0], ids[1])
	}
	if ids[2] != ids[3] {
		t.Fatalf("message_start/message_end id mismatch for second UserMessage: %q vs %q", ids[2], ids[3])
	}
	if ids[0] == ids[2] {
		t.Fatalf("two distinct UserMessage calls produced the same id %q; cache dedup would collapse them", ids[0])
	}
}

// TestMapAssistantMessage_MappingRules exercises the mapping rules the Task 6
// brief calls out explicitly, each as an independent subtest so a wrong
// mapping fails with a precise message.
func TestMapAssistantMessage_MappingRules(t *testing.T) {
	t.Run("thinking block maps to PiThinkingBlock", func(t *testing.T) {
		const resp = `{
 "id":"msg_t","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"thinking","thinking":"pondering the mysteries"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		if len(mapped.Content) != 1 {
			t.Fatalf("content = %+v, want 1 block", mapped.Content)
		}
		if mapped.Content[0].Type != "thinking" {
			t.Fatalf("content[0].type = %q, want thinking", mapped.Content[0].Type)
		}
		if mapped.Content[0].Thinking != "pondering the mysteries" {
			t.Fatalf("content[0].thinking = %q", mapped.Content[0].Thinking)
		}
		b, err := json.Marshal(mapped.Content[0])
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["thinking"]; !ok {
			t.Fatalf("marshaled thinking block missing thinking key: %s", b)
		}
	})

	t.Run("max_tokens stop reason maps to length", func(t *testing.T) {
		const resp = `{
 "id":"msg_m","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"max_tokens",
 "content":[{"type":"text","text":"cut off"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		if mapped.StopReason != "length" {
			t.Fatalf("stopReason = %q, want length", mapped.StopReason)
		}
	})

	t.Run("cache read/write values and total token sum", func(t *testing.T) {
		const resp = `{
 "id":"msg_u","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":7,"output_tokens":11,"cache_read_input_tokens":13,"cache_creation_input_tokens":17}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		if mapped.Usage.Input != 7 || mapped.Usage.Output != 11 {
			t.Fatalf("input/output = %d/%d, want 7/11", mapped.Usage.Input, mapped.Usage.Output)
		}
		if mapped.Usage.CacheRead != 13 {
			t.Fatalf("cacheRead = %d, want 13", mapped.Usage.CacheRead)
		}
		if mapped.Usage.CacheWrite != 17 {
			t.Fatalf("cacheWrite = %d, want 17", mapped.Usage.CacheWrite)
		}
		wantTotal := 7 + 11 + 13 + 17
		if mapped.Usage.TotalTokens != wantTotal {
			t.Fatalf("totalTokens = %d, want %d", mapped.Usage.TotalTokens, wantTotal)
		}
	})

	t.Run("API and Provider are set from constant and argument", func(t *testing.T) {
		const resp = `{
 "id":"msg_p","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "some-custom-provider")
		if mapped.API != "anthropic-messages" {
			t.Fatalf("API = %q, want anthropic-messages", mapped.API)
		}
		if mapped.Provider != "some-custom-provider" {
			t.Fatalf("Provider = %q, want some-custom-provider", mapped.Provider)
		}
	})

	t.Run("cost stays zero", func(t *testing.T) {
		const resp = `{
 "id":"msg_c","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":42,"output_tokens":99,"cache_read_input_tokens":5,"cache_creation_input_tokens":6}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		c := mapped.Usage.Cost
		if c.Input != 0 || c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0 || c.Total != 0 {
			t.Fatalf("cost = %+v, want all-zero (unknown at this layer)", c)
		}
	})

	t.Run("tool_use raw fallback on unmarshal failure", func(t *testing.T) {
		silenceSlog(t)
		const resp = `{
 "id":"msg_r","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_9","name":"weird","input":[1,2,3]}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		if len(mapped.Content) != 1 || mapped.Content[0].Type != "toolCall" {
			t.Fatalf("content = %+v, want one toolCall block", mapped.Content)
		}
		args := mapped.Content[0].Arguments
		if args == nil {
			t.Fatal("arguments is nil")
		}
		raw, ok := (*args)["_raw"]
		if !ok {
			t.Fatalf("arguments missing _raw fallback key: %+v", *args)
		}
		if raw != "[1,2,3]" {
			t.Fatalf("_raw = %v, want the literal unparsed input %q", raw, "[1,2,3]")
		}
	})

	t.Run("skips empty text and thinking blocks", func(t *testing.T) {
		const resp = `{
 "id":"msg_e","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"thinking","thinking":""},
            {"type":"text","text":"hi"},
            {"type":"text","text":""}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(resp), &msg); err != nil {
			t.Fatal(err)
		}
		mapped := MapAssistantMessage(&msg, "anthropic")
		if len(mapped.Content) != 1 {
			t.Fatalf("content = %+v, want exactly the 1 non-empty text block", mapped.Content)
		}
		if mapped.Content[0].Type != "text" || mapped.Content[0].Text != "hi" {
			t.Fatalf("content[0] = %+v, want text %q", mapped.Content[0], "hi")
		}
	})
}

// TestToolStart_RawFallback covers the ToolStart side of the _raw fallback:
// when input can't be unmarshaled into map[string]any, the raw bytes must be
// preserved under an "_raw" key rather than silently dropped.
func TestToolStart_RawFallback(t *testing.T) {
	silenceSlog(t)
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic")
	em.ToolStart("tu_x", "mytool", json.RawMessage("not-json"))

	line := strings.TrimSpace(out.String())
	var frame struct {
		Type string         `json:"type"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("bad frame %q: %v", line, err)
	}
	if frame.Type != "tool_execution_start" {
		t.Fatalf("type = %q, want tool_execution_start", frame.Type)
	}
	raw, ok := frame.Args["_raw"]
	if !ok {
		t.Fatalf("args missing _raw fallback key: %+v", frame.Args)
	}
	if raw != "not-json" {
		t.Fatalf("_raw = %v, want %q", raw, "not-json")
	}
}
