package fundi

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

	"go.graveland.dev/rafiki/pkg/child"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
	var acc anthropic.Message
	for _, ev := range evs {
		var u anthropic.MessageStreamEventUnion
		c.NoError(json.Unmarshal(ev.Data, &u), "unmarshal stream event %s", ev.Type)
		c.NoError(acc.Accumulate(u), "accumulate stream event %s", ev.Type)
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
	c := assert.NewCollecting(t)
	acc := accumulateSDKEvents(t, accumulateTextEvents("Hel", "lo"))
	got := MapAssistantMessage(acc, "anthropic", nil)
	c.Require().Len(got.Content, 1, "content")
	c.Eq("Hello", got.Content[0].Text, "text")
}

// TestMapAssistantMessage_MapsAccumulatedToolUseBlock is
// TestMapAssistantMessage_MapsAccumulatedTextBlock's tool_use sibling: every
// As*() reads JSON.raw identically, so a still-accumulating tool_use block
// vanished from message_update the same way a text block did.
func TestMapAssistantMessage_MapsAccumulatedToolUseBlock(t *testing.T) {
	c := assert.NewAborting(t)
	acc := accumulateSDKEvents(t, accumulateToolUseEvents("call-1", "bash", `{"command":"ls"}`))
	got := MapAssistantMessage(acc, "anthropic", nil)
	c.Len(got.Content, 1, "content")
	c.Eq("toolCall", got.Content[0].Type, "content[0].type")
	c.NotNil(got.Content[0].Arguments, "arguments is nil")
	if cmd := (*got.Content[0].Arguments)["command"]; cmd != "ls" {
		t.Fatalf("arguments = %+v, want command=ls", *got.Content[0].Arguments)
	}
}

// TestMapAssistantMessage_ToolUsePartialInputDoesNotWarnOrTruncate is H6: a
// mid-stream flush must not warn, and must not surface a truncated JSON
// fragment as {"_raw": ...}. This is the fresh regression from d53d5dc:
// MapAssistantMessage now reads b.Input directly (correctly, per
// TestMapAssistantMessage_MapsAccumulatedToolUseBlock above) and unmarshals
// it, but per anthropic.Message.Accumulate a tool_use block's Input is a
// growing JSON *prefix* until content_block_stop, not a complete document --
// so every flush before that point handed json.Unmarshal a truncated
// fragment, which is neither empty nor valid, and previously always took the
// warn+_raw path meant for GENUINELY malformed complete input.
//
// accumulateToolUseEvents (used by TestMapAssistantMessage_MapsAccumulatedToolUseBlock)
// feeds its whole input as ONE input_json_delta, which Accumulate replaces
// "{}" with wholesale -- that is always complete JSON and never exercises a
// truncated state, which is exactly why the brief calls that guard vacuous.
// This test instead splits `{"file_path": "/Users/brent/project"}` across
// TWO deltas and asserts against the state after only the first has landed
// -- a genuine SDK-accumulated prefix, not a hand-typed guess at what one
// looks like.
func TestMapAssistantMessage_ToolUsePartialInputDoesNotWarnOrTruncate(t *testing.T) {
	c := assert.NewAborting(t)
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	acc := accumulateSDKEvents(t, []ssestream.Event{
		streamMessageStart("claude-x"),
		streamToolUseStart(0, "tu_partial", "Write"),
		streamInputJSONDelta(0, `{"file_path": "/Users/b`),
	})
	if json.Valid(acc.Content[0].Input) {
		t.Fatalf("test fixture bug: b.Input = %q must be an INCOMPLETE JSON prefix, not valid JSON",
			acc.Content[0].Input)
	}

	got := MapAssistantMessage(acc, "anthropic", nil)

	c.Eq("", logs.String(), "logged a warning for input that is merely still streaming")
	c.False(len(got.Content) != 1 || got.Content[0].Type != "toolCall", "content = %+v, want one toolCall block (never dropped mid-stream, to avoid TUI flicker)", got.Content)
	args := got.Content[0].Arguments
	c.NotNil(args, "arguments is nil")
	if _, isRaw := (*args)["_raw"]; isRaw {
		t.Fatalf("arguments leaked the truncated fragment as _raw: %+v", *args)
	}
	c.Empty(*args, "arguments")

	// The second delta completes the JSON. The next flush (the real shape of
	// engine.go's stream handler: MapAssistantMessage is called again against
	// the same accumulator, not handed a delta directly) must map the full,
	// correct arguments and still log nothing.
	var u anthropic.MessageStreamEventUnion
	ev2 := streamInputJSONDelta(0, `rent/project"}`)
	c.NoError(json.Unmarshal(ev2.Data, &u), "unmarshal second delta")
	c.NoError(acc.Accumulate(u), "accumulate second delta")
	if !json.Valid(acc.Content[0].Input) {
		t.Fatalf("test fixture bug: b.Input = %q must be complete valid JSON after the second delta",
			acc.Content[0].Input)
	}

	got2 := MapAssistantMessage(acc, "anthropic", nil)
	c.Eq("", logs.String(), "logged a warning after the second delta completed valid JSON")
	args2 := got2.Content[0].Arguments
	c.False(args2 == nil || (*args2)["file_path"] != "/Users/brent/project", "arguments after completion = %+v, want file_path=/Users/brent/project", args2)
}

const sampleResp = `{
 "id":"msg_1","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"text","text":"on it"},
            {"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`

func TestAssistantTurnEmitsPiFrames(t *testing.T) {
	c := assert.NewAborting(t)
	var resp anthropic.Message
	c.NoError(json.Unmarshal([]byte(sampleResp), &resp))
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic", nil)
	em.AgentStart()
	em.UserMessage("go", nil)
	em.AssistantTurn(&resp)
	em.ToolStart("tu_1", "bash", json.RawMessage(`{"command":"ls"}`))
	em.ToolEnd("tu_1", "bash", "file.txt", false)
	em.AgentEnd()

	var types []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			Type string `json:"type"`
		}
		c.NoError(json.Unmarshal([]byte(l), &f), "bad frame %q", l)
		types = append(types, f.Type)
	}
	want := []string{"agent_start", "message_start", "message_end", // user echo
		"message_start", "message_update", "message_end", // assistant
		"tool_execution_start", "tool_execution_end",
		"agent_end", "agent_settled"}
	c.Eq(strings.Join(want, ","), strings.Join(types, ","), "frame sequence:\n got %v\nwant %v", types, want)
	// spot-check mapping on the assistant message_end frame
	var me struct {
		Message struct {
			StopReason string           `json:"stopReason"`
			Content    []map[string]any `json:"content"`
			Usage      struct{ Input, Output int }
		} `json:"message"`
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.NoError(json.Unmarshal([]byte(lines[5]), &me), "unmarshal message_end frame")
	c.Eq("toolUse", me.Message.StopReason, "stopReason")
	if me.Message.Content[1]["type"] != "toolCall" {
		t.Fatalf("content[1]: %v", me.Message.Content[1])
	}
	c.False(me.Message.Usage.Input != 10 || me.Message.Usage.Output != 5, "usage: %+v", me.Message.Usage)
	// agent_end carries the 3 accumulated messages: user echo, assistant, toolResult
	var ae struct {
		Messages []json.RawMessage `json:"messages"`
	}
	c.NoError(json.Unmarshal([]byte(lines[8]), &ae), "unmarshal agent_end frame")
	c.Len(ae.Messages, 3, "agent_end messages: %d", len(ae.Messages))
}

// TestMapAssistantMessageEmptyContentIsEmptyArray guards against a nil
// Content slice marshaling as JSON null: the pi TUI expects content to
// always be an array, even when a response yields no mappable blocks (e.g.
// only block types this mapper doesn't handle yet).
func TestMapAssistantMessageEmptyContentIsEmptyArray(t *testing.T) {
	c := assert.NewAborting(t)
	const resp = `{
 "id":"msg_2","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	var msg anthropic.Message
	c.NoError(json.Unmarshal([]byte(resp), &msg))
	mapped := MapAssistantMessage(&msg, "anthropic", nil)
	b, err := json.Marshal(mapped)
	c.NoError(err)
	var raw map[string]json.RawMessage
	c.NoError(json.Unmarshal(b, &raw))
	c.Eq("[]", string(raw["content"]), "content = %s, want []", raw["content"])
	c.Eq("stop", mapped.StopReason, "stopReason")
}

// TestUserMessageAssignsUniqueID guards the pi consumer's message_end dedup
// contract: internal/child/pi_events.go documents that the consumer appends
// on message_end "deduping by id" (see claudeUserEcho's ID: fmt.Sprintf
// ("user-%d", ts) precedent). An always-empty ID would collide every user
// turn in the cache.
func TestUserMessageAssignsUniqueID(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic", nil)
	em.UserMessage("first", nil)
	// A real turn always separates two UserMessage calls by at least one LLM
	// round trip; sleep to guarantee distinct millisecond timestamps rather
	// than asserting uniqueness at zero elapsed time, which the ts-based
	// scheme (matching the claudeUserEcho precedent) was never meant to give.
	time.Sleep(2 * time.Millisecond)
	em.UserMessage("second", nil)

	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		c.NoError(json.Unmarshal([]byte(l), &f), "bad frame %q", l)
		ids = append(ids, f.Message.ID)
	}
	// message_start + message_end for each of 2 UserMessage calls = 4 frames.
	c.Len(ids, 4, "got %d frames, want 4", len(ids))
	for _, id := range ids {
		c.NotEq("", id, "frame has empty message id: %v", ids)
	}
	c.Eq(ids[1], ids[0], "message_start/message_end id mismatch for first UserMessage")
	c.Eq(ids[3], ids[2], "message_start/message_end id mismatch for second UserMessage")
	c.NotEq(ids[2], ids[0], "two distinct UserMessage calls produced the same id")
}

// TestMapAssistantMessage_MappingRules exercises the mapping rules the Task 6
// brief calls out explicitly, each as an independent subtest so a wrong
// mapping fails with a precise message.
func TestMapAssistantMessage_MappingRules(t *testing.T) {
	t.Run("thinking block maps to PiThinkingBlock", func(t *testing.T) {
		c := assert.NewAborting(t)
		const resp = `{
 "id":"msg_t","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"thinking","thinking":"pondering the mysteries"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c.Len(mapped.Content, 1, "content")
		c.Eq("thinking", mapped.Content[0].Type, "content[0].type")
		c.Eq("pondering the mysteries", mapped.Content[0].Thinking, "content[0].thinking =")
		b, err := json.Marshal(mapped.Content[0])
		c.NoError(err)
		var raw map[string]json.RawMessage
		c.NoError(json.Unmarshal(b, &raw))
		_, ok := raw["thinking"]
		c.True(ok, "marshaled thinking block missing thinking key: %s", b)
	})

	t.Run("max_tokens stop reason maps to length", func(t *testing.T) {
		c := assert.NewAborting(t)
		const resp = `{
 "id":"msg_m","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"max_tokens",
 "content":[{"type":"text","text":"cut off"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c.Eq("length", mapped.StopReason, "stopReason")
	})

	t.Run("cache read/write values and total token sum", func(t *testing.T) {
		c := assert.NewAborting(t)
		const resp = `{
 "id":"msg_u","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":7,"output_tokens":11,"cache_read_input_tokens":13,"cache_creation_input_tokens":17}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c.False(mapped.Usage.Input != 7 || mapped.Usage.Output != 11, "input/output = %d/%d, want 7/11", mapped.Usage.Input, mapped.Usage.Output)
		c.Eq(13, mapped.Usage.CacheRead, "cacheRead")
		c.Eq(17, mapped.Usage.CacheWrite, "cacheWrite")
		wantTotal := 7 + 11 + 13 + 17
		c.Eq(wantTotal, mapped.Usage.TotalTokens, "totalTokens")
	})

	t.Run("API and Provider are set from constant and argument", func(t *testing.T) {
		c := assert.NewAborting(t)
		const resp = `{
 "id":"msg_p","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "some-custom-provider", nil)
		c.Eq("anthropic-messages", mapped.API, "API")
		c.Eq("some-custom-provider", mapped.Provider, "Provider")
	})

	t.Run("cost stays zero", func(t *testing.T) {
		ck := assert.NewAborting(t)
		const resp = `{
 "id":"msg_c","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"ok"}],
 "usage":{"input_tokens":42,"output_tokens":99,"cache_read_input_tokens":5,"cache_creation_input_tokens":6}}`
		var msg anthropic.Message
		ck.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c := mapped.Usage.Cost
		ck.False(c.Input != 0 || c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0 || c.Total != 0, "cost = %+v, want all-zero (unknown at this layer)", c)
	})

	t.Run("tool_use raw fallback on unmarshal failure", func(t *testing.T) {
		c := assert.NewAborting(t)
		silenceSlog(t)
		const resp = `{
 "id":"msg_r","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_9","name":"weird","input":[1,2,3]}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c.False(len(mapped.Content) != 1 || mapped.Content[0].Type != "toolCall", "content = %+v, want one toolCall block", mapped.Content)
		args := mapped.Content[0].Arguments
		c.NotNil(args, "arguments is nil")
		raw, ok := (*args)["_raw"]
		if !ok {
			t.Fatalf("arguments missing _raw fallback key: %+v", *args)
		}
		c.False(raw != "[1,2,3]", "_raw = %v, want the literal unparsed input %q", raw, "[1,2,3]")
	})

	t.Run("skips empty text and thinking blocks", func(t *testing.T) {
		c := assert.NewAborting(t)
		const resp = `{
 "id":"msg_e","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"thinking","thinking":""},
            {"type":"text","text":"hi"},
            {"type":"text","text":""}],
 "usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
		var msg anthropic.Message
		c.NoError(json.Unmarshal([]byte(resp), &msg))
		mapped := MapAssistantMessage(&msg, "anthropic", nil)
		c.Len(mapped.Content, 1, "content")
		if mapped.Content[0].Type != "text" || mapped.Content[0].Text != "hi" {
			t.Fatalf("content[0] = %+v, want text %q", mapped.Content[0], "hi")
		}
	})
}

// TestToolStart_RawFallback covers the ToolStart side of the _raw fallback:
// when input can't be unmarshaled into map[string]any, the raw bytes must be
// preserved under an "_raw" key rather than silently dropped.
func TestToolStart_RawFallback(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(json.Unmarshal([]byte(line), &frame), "bad frame %q", line)
	c.Eq("tool_execution_start", frame.Type, "type")
	raw, ok := frame.Args["_raw"]
	c.True(ok, "args missing _raw fallback key: %+v", frame.Args)
	c.False(raw != "not-json", "_raw = %v, want %q", raw, "not-json")
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
	n := countOfType(types, "message_start")
	assert.NewAborting(t).Eq(1, n, "message_start emitted %d times, want 1: %v", n, types)
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
	n := countOfType(types, "message_start")
	assert.NewAborting(t).Eq(2, n, "message_start emitted %d times across two turns, want 2: %v", n, types)
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
	assert.NewAborting(t).Eq(2, countOfType(types, "message_start"), "message_start emitted")
}

// TestStreamSequence_OrdersStartUpdatesEnd locks down frame ordering: a
// StreamStart, N StreamDeltas, then StreamEnd must produce exactly
// message_start, N message_update, ONE MORE message_update (StreamEnd's own
// final-state flush — see its doc comment for why that extra update is not
// redundant), then message_end, in that order.
func TestStreamSequence_OrdersStartUpdatesEnd(t *testing.T) {
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamDelta(msg)
	e.StreamDelta(msg)
	e.StreamEnd(msg)

	assertFrameTypes(t, out.String(),
		[]string{"message_start", "message_update", "message_update", "message_update", "message_end"})
}

// TestStreamEnd_FinalUpdateCarriesCompleteArgsEvenWithNoInterveningDelta is
// the regression this file exists to guard: a fast tool call whose whole turn
// completes inside one streamFlushInterval window gets exactly ONE flush (at
// StreamStart, before the tool_use block's JSON input has finished
// accumulating) and then goes straight to StreamEnd with no StreamDelta in
// between — the exact shape the real bug had. pi's TUI reads a tool call's
// arguments from message_update.content[].arguments and never re-reads them
// from message_end, so if StreamEnd's flush isn't there, the args a client
// ever sees stay permanently empty even though the tool executed correctly
// end to end (a real fundi turn against moonshotai/kimi-k3 rendered "$ ..."
// with no command text in pi's attach TUI, though tool_execution_start/end
// and the persisted transcript were both fine).
func TestStreamEnd_FinalUpdateCarriesCompleteArgsEvenWithNoInterveningDelta(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)

	early := child.PiAssistantMessage{Role: "assistant", Content: []child.PiContentBlock{
		child.PiToolCallBlock("call-1", "bash", nil), // input JSON hasn't accumulated yet
	}}
	final := child.PiAssistantMessage{Role: "assistant", Content: []child.PiContentBlock{
		child.PiToolCallBlock("call-1", "bash", map[string]any{"command": "date +%s"}),
	}}

	e.StreamStart(early) // message_start with empty arguments
	e.StreamEnd(final)   // NO StreamDelta in between — the bug's exact shape

	types := frameTypes(t, out.String())
	want := []string{"message_start", "message_update", "message_end"}
	c.Eq(strings.Join(want, ","), strings.Join(types, ","), "frame sequence = %v, want %v", types, want)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var update struct {
		Message child.PiAssistantMessage `json:"message"`
	}
	c.NoError(json.Unmarshal([]byte(lines[1]), &update), "unmarshal message_update") // message_update is frame index 1
	args := update.Message.Content[0].Arguments
	if args == nil || (*args)["command"] != "date +%s" {
		t.Fatalf("message_update args = %+v, want command=%q — the client's only source for a live tool call's "+
			"arguments never saw the complete input", args, "date +%s")
	}
}

// TestStreamDelta_DoesNotAccumulateOrFoldUsage guards against a delta being
// mistaken for the terminal message: only StreamEnd's message may end up in
// agent_end's messages[] and usage total, or a multi-delta turn would
// over-count both.
func TestStreamDelta_DoesNotAccumulateOrFoldUsage(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(json.Unmarshal([]byte(lines[len(lines)-2]), &ae), "unmarshal agent_end frame")
	c.Len(ae.Messages, 1, "agent_end messages = %d, want 1 (only StreamEnd's message)", len(ae.Messages))
	c.Eq(15, ae.Usage.TotalTokens, "agent_end totalTokens")
}

// TestEmitterBatchWaitFrames pins the batch_wait frame pair: each method
// emits exactly its one-key frame through the same Frontend path AgentStart
// uses, so pkg/child's state machine sees a bare batch_wait_start/
// batch_wait_end event and nothing else (task 3.1 wires the callers).
func TestEmitterBatchWaitFrames(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	e := NewEmitter(fe, "anthropic", nil)

	e.AgentStart()
	e.BatchWaitStart()
	e.BatchWaitEnd()

	types := frameTypes(t, out.String())
	want := []string{"agent_start", "batch_wait_start", "batch_wait_end"}
	c.Eq(strings.Join(want, ","), strings.Join(types, ","), "frame sequence = %v, want %v", types, want)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i, wantType := range want[1:] {
		var raw map[string]any
		c.NoError(json.Unmarshal([]byte(lines[i+1]), &raw), "unmarshal frame %q", lines[i+1])
		c.Len(raw, 1, "%s frame = %v, want only the type key", wantType, raw)
	}
}
