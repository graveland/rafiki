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
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"git.graveland.dev/brent/fundi/internal/child"
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

// accumulateTextEvents builds the SDK's own stream events for a text block
// that has started and received deltas but has NOT reached
// content_block_stop -- the exact state MapAssistantMessage is handed on
// every message_update (engine.go's stream handler maps the accumulator on
// every content_block_delta, well before that block's content_block_stop,
// which is the only thing that resyncs ContentBlockUnion.JSON.raw -- see
// anthropic.Message.Accumulate in messageutil.go). Built from the same
// SDK-shaped event constructors engine_stream_test.go uses
// (streamMessageStart/streamTextBlockStart/streamTextDelta) rather than a
// parallel fixture builder.
func accumulateTextEvents(parts ...string) []ssestream.Event {
	ev := []ssestream.Event{streamMessageStart("claude-x"), streamTextBlockStart(0)}
	for _, p := range parts {
		ev = append(ev, streamTextDelta(0, p))
	}
	return ev
}

// accumulateToolUseEvents is accumulateTextEvents' tool_use sibling: a
// content_block_start followed by input_json_delta fragments, again with no
// content_block_stop -- the mid-stream state a message_update sees for a
// still-open tool_use block.
func accumulateToolUseEvents(toolID, name string, jsonParts ...string) []ssestream.Event {
	ev := []ssestream.Event{streamMessageStart("claude-x"), streamToolUseStart(0, toolID, name)}
	for _, p := range jsonParts {
		ev = append(ev, streamInputJSONDelta(0, p))
	}
	return ev
}

// accumulateSDKEvents replays evs into a fresh anthropic.Message via the real
// SDK Accumulate method, unmarshaling each event's Data exactly the way
// ssestream.Stream[T] does before handing it to the caller (see
// packages/ssestream/ssestream.go: json.Unmarshal(s.decoder.Event().Data,
// &nxt)) -- so this is the real accumulation path, not a simplified stand-in.
func accumulateSDKEvents(t *testing.T, evs []ssestream.Event) *anthropic.Message {
	t.Helper()
	var acc anthropic.Message
	for _, ev := range evs {
		var u anthropic.MessageStreamEventUnion
		if err := json.Unmarshal(ev.Data, &u); err != nil {
			t.Fatalf("unmarshal stream event %s: %v", ev.Type, err)
		}
		if err := acc.Accumulate(u); err != nil {
			t.Fatalf("accumulate stream event %s: %v", ev.Type, err)
		}
	}
	return &acc
}

// TestMapAssistantMessage_MapsAccumulatedTextBlock guards the bug this
// project shipped once: MapAssistantMessage dispatched via b.AsAny(), which
// reconstructs the block from ContentBlockUnion.JSON.raw. Message.Accumulate
// never rewrites that raw JSON while a block is still open -- only
// content_block_stop/message_stop resync it -- it grows the struct field in
// place instead. So every streamed message_update mapped to empty content
// while hasContent (which reads the field directly) correctly saw text and
// flushed: 23 empty frames per turn, and the full reply only in message_end.
func TestMapAssistantMessage_MapsAccumulatedTextBlock(t *testing.T) {
	acc := accumulateSDKEvents(t, accumulateTextEvents("Hel", "lo"))
	got := MapAssistantMessage(acc, "anthropic", nil)
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want one text block -- an accumulated (not API-parsed) message must still map", got.Content)
	}
	if got.Content[0].Text != "Hello" {
		t.Errorf("text = %q, want %q", got.Content[0].Text, "Hello")
	}
}

// TestMapAssistantMessage_MapsAccumulatedToolUseBlock is
// TestMapAssistantMessage_MapsAccumulatedTextBlock's tool_use sibling: every
// As*() reads JSON.raw identically, so a still-accumulating tool_use block
// vanished from message_update the same way a text block did.
func TestMapAssistantMessage_MapsAccumulatedToolUseBlock(t *testing.T) {
	acc := accumulateSDKEvents(t, accumulateToolUseEvents("call-1", "bash", `{"command":"ls"}`))
	got := MapAssistantMessage(acc, "anthropic", nil)
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v, want one tool_use block", got.Content)
	}
	if got.Content[0].Type != "toolCall" {
		t.Fatalf("content[0].type = %q, want toolCall", got.Content[0].Type)
	}
	if got.Content[0].Arguments == nil {
		t.Fatal("arguments is nil")
	}
	if cmd := (*got.Content[0].Arguments)["command"]; cmd != "ls" {
		t.Fatalf("arguments = %+v, want command=ls", *got.Content[0].Arguments)
	}
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
	em := NewEmitter(fe, "anthropic", nil)
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
	mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
	em := NewEmitter(fe, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "some-custom-provider", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
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
	em := NewEmitter(fe, "anthropic", nil)
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

// countOfType counts how many entries in types equal want. types is produced
// by engine_test.go's frameTypes(t, out string) helper, reused here rather
// than duplicated (there is no fakeFrontend in this package — tests drive a
// real Frontend backed by a bytes.Buffer).
func countOfType(types []string, want string) int {
	n := 0
	for _, ty := range types {
		if ty == want {
			n++
		}
	}
	return n
}

// TestStreamStart_EmitsOnlyOnce locks down the §0.2 idempotency guard: a
// second StreamStart before StreamEnd must not emit a second message_start,
// or a sendWithTrim-style retry that calls StreamStart again would duplicate
// the frame in an attached TUI.
func TestStreamStart_EmitsOnlyOnce(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamStart(msg)

	types := frameTypes(t, out.String())
	if n := countOfType(types, "message_start"); n != 1 {
		t.Fatalf("message_start emitted %d times, want 1: %v", n, types)
	}
}

// TestStreamEnd_ResetsSoNextTurnStartsAgain locks down that StreamEnd resets
// the started guard: a StreamStart in a later turn (after a StreamEnd) must
// emit again, or every turn after the first would silently lose its
// message_start.
func TestStreamEnd_ResetsSoNextTurnStartsAgain(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamEnd(msg)
	e.StreamStart(msg)

	types := frameTypes(t, out.String())
	if n := countOfType(types, "message_start"); n != 2 {
		t.Fatalf("message_start emitted %d times across two turns, want 2: %v", n, types)
	}
}

// TestAgentEndResetsStartedSoNextTurnEmitsMessageStart locks down that
// AgentEnd resets the started guard, not just StreamEnd: a stream that fails
// or is aborted AFTER content has been emitted never reaches StreamEnd (see
// engine.go's OnTurn, which returns on err before calling StreamEnd), so
// AgentEnd is the only place left in that path to reset it. If `started`
// survives AgentEnd, the next turn's StreamStart silently no-ops and the
// emitter is permanently one message_start in debt.
func TestAgentEndResetsStartedSoNextTurnEmitsMessageStart(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	// Turn 1: content streamed, then the turn tears down without StreamEnd
	// (mid-stream abort or a post-content failure).
	e.StreamStart(msg)
	e.StreamDelta(msg)
	e.AgentEnd()

	// Turn 2: a completely healthy streamed turn.
	e.StreamStart(msg)

	types := frameTypes(t, out.String())
	if n := countOfType(types, "message_start"); n != 2 {
		t.Fatalf("message_start emitted %d times across two turns, want 2 — `started` leaked past AgentEnd", n)
	}
}

// TestStreamSequence_OrdersStartUpdatesEnd locks down frame ordering: a
// StreamStart, N StreamDeltas, then StreamEnd must produce exactly
// message_start, N message_update, message_end in that order.
func TestStreamSequence_OrdersStartUpdatesEnd(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamDelta(msg)
	e.StreamDelta(msg)
	e.StreamEnd(msg)

	assertFrameTypes(t, out.String(), []string{"message_start", "message_update", "message_update", "message_end"})
}

// TestStreamDelta_DoesNotAccumulateOrFoldUsage guards against a delta being
// mistaken for the terminal message: only StreamEnd's message may end up in
// agent_end's messages[] and usage total, or a multi-delta turn would
// over-count both.
func TestStreamDelta_DoesNotAccumulateOrFoldUsage(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant", Usage: child.PiUsage{Input: 10, Output: 5, TotalTokens: 15}}

	e.AgentStart()
	e.StreamStart(msg)
	e.StreamDelta(msg)
	e.StreamDelta(msg)
	e.StreamDelta(msg)
	e.StreamEnd(msg)
	e.AgentEnd()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var ae struct {
		Messages []json.RawMessage `json:"messages"`
		Usage    child.PiUsage     `json:"usage"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-2]), &ae); err != nil {
		t.Fatalf("unmarshal agent_end frame: %v", err)
	}
	if len(ae.Messages) != 1 {
		t.Fatalf("agent_end messages = %d, want 1 (only StreamEnd's message)", len(ae.Messages))
	}
	if ae.Usage.TotalTokens != 15 {
		t.Fatalf("agent_end totalTokens = %d, want 15 (folded once, not once per delta)", ae.Usage.TotalTokens)
	}
}
