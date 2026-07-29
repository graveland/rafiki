package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// ---- streaming event fixtures ----
//
// These mirror rafiki's own (package-private) llm/conversation_stream_test.go
// fixtures, rebuilt here against the exported llm.StreamingSender interface
// so the engine's streaming wiring (Task B4) can be driven end to end with no
// network. Event bodies for the tool_use case are lifted from a real
// recorded API response (anthropic-sdk-go's
// toolrunner/testdata/cassettes/tool_runner_next_streaming.yaml) rather than
// invented, so the input_json_delta fragmentation (including the
// "{}"-replaced-not-appended first chunk — see anthropic.Message.Accumulate)
// matches what the real API actually sends.

func sseEvt(typ, raw string) ssestream.Event {
	return ssestream.Event{Type: typ, Data: []byte(raw)}
}

func streamMessageStart(model string) ssestream.Event {
	return sseEvt("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":%q,`+
			`"content":[],"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		model))
}

func streamTextBlockStart(index int) ssestream.Event {
	return sseEvt("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index))
}

func streamTextDelta(index int, text string) ssestream.Event {
	b, _ := json.Marshal(text)
	return sseEvt("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, b))
}

func streamToolUseStart(index int, id, name string) ssestream.Event {
	return sseEvt("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`,
		index, id, name))
}

func streamInputJSONDelta(index int, partial string) ssestream.Event {
	b, _ := json.Marshal(partial)
	return sseEvt("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, b))
}

func streamBlockStop(index int) ssestream.Event {
	return sseEvt("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func streamMessageDelta(stopReason string, outputTokens int) ssestream.Event {
	return sseEvt("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stopReason, outputTokens))
}

func streamMessageStop() ssestream.Event {
	return sseEvt("message_stop", `{"type":"message_stop"}`)
}

// textTurnEvents scripts a complete single-text-block streamed turn ending in
// end_turn, split across the given deltas.
func textTurnEvents(model string, parts ...string) []ssestream.Event {
	ev := []ssestream.Event{streamMessageStart(model), streamTextBlockStart(0)}
	for _, p := range parts {
		ev = append(ev, streamTextDelta(0, p))
	}
	ev = append(ev, streamBlockStop(0), streamMessageDelta("end_turn", len(parts)), streamMessageStop())
	return ev
}

// toolUseTurnEvents scripts a streamed turn whose only content is a single
// tool_use block, its input arriving as jsonParts input_json_delta fragments
// (see the cassette-derived fragmentation note above) — the shape needed to
// prove tool dispatch waits for the fully reassembled JSON.
func toolUseTurnEvents(model, toolID, toolName string, jsonParts []string) []ssestream.Event {
	ev := []ssestream.Event{streamMessageStart(model), streamToolUseStart(0, toolID, toolName)}
	for _, p := range jsonParts {
		ev = append(ev, streamInputJSONDelta(0, p))
	}
	ev = append(ev, streamBlockStop(0), streamMessageDelta("tool_use", 10), streamMessageStop())
	return ev
}

// preContentFailureEvents scripts the SDK's own structural events
// (message_start, an empty content_block_start) with NO content ever
// delivered, followed by a decoder-surfaced error — modeling a mid-stream
// failure that happens before any real content arrives. hasContent must gate
// the handler so this never fires StreamStart.
func preContentFailureEvents() []ssestream.Event {
	return []ssestream.Event{streamMessageStart("claude-x"), streamTextBlockStart(0)}
}

// fakeStreamDecoder replays a fixed event slice, then (once exhausted)
// surfaces trailErr from Err() — mirroring rafiki's own fakeDecoder in
// llm/conversation_stream_test.go (unexported there, so rebuilt here).
type fakeStreamDecoder struct {
	events   []ssestream.Event
	idx      int
	trailErr error
}

func (d *fakeStreamDecoder) Next() bool {
	if d.idx >= len(d.events) {
		return false
	}
	d.idx++
	return true
}
func (d *fakeStreamDecoder) Event() ssestream.Event { return d.events[d.idx-1] }
func (d *fakeStreamDecoder) Close() error           { return nil }
func (d *fakeStreamDecoder) Err() error             { return d.trailErr }

// streamScript is one scripted NewStreaming call, mirroring rafiki's own
// streamScript (unexported there).
type streamScript struct {
	events   []ssestream.Event
	trailErr error
}

// scriptedStreamingSender implements llm.StreamingSender, replaying queued
// streamScripts in order — one per agentloop.drive iteration. New must never
// be called: every conv.Continue in the engine always carries
// llm.WithStreamHandler, so sendAttempt always tries sendStreaming first, and
// this sender always reports canStream=true; a call to New is therefore a
// test bug (streaming silently fell back), not a legitimate path.
type scriptedStreamingSender struct {
	mu      sync.Mutex
	scripts []streamScript
	calls   int
}

func newScriptedStreamingSender(scripts ...streamScript) *scriptedStreamingSender {
	return &scriptedStreamingSender{scripts: scripts}
}

func (s *scriptedStreamingSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, errors.New("scriptedStreamingSender: New called; want NewStreaming (a stream handler is always set)")
}

func (s *scriptedStreamingSender) NewStreaming(_ context.Context, _ anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.scripts) {
		return nil, errors.New("scriptedStreamingSender: streaming scripts exhausted")
	}
	sc := s.scripts[s.calls]
	s.calls++
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](
		&fakeStreamDecoder{events: sc.events, trailErr: sc.trailErr}, nil), nil
}

// ---- tests ----

// TestEngine_StreamsDeltasAndPricesFinalMessageOnce proves the streaming
// path: a StreamingSender's events must produce message_update frames as
// they arrive, then a single message_end carrying the fully accumulated
// text, priced exactly once (identical to what AssistantTurn would have
// produced for the same final message — see emit_cost_test.go's
// TestStreamEndFoldsCostIdenticallyToAssistantTurn for that half of the
// guarantee).
func TestEngine_StreamsDeltasAndPricesFinalMessageOnce(t *testing.T) {
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{events: textTurnEvents("claude-x", "Hel", "lo")})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	types := frameTypes(t, out.String())
	want := []string{
		"message_start", "message_end", // user echo
		"agent_start",
	}
	if !strings.HasPrefix(strings.Join(types, ","), strings.Join(want, ",")) {
		t.Fatalf("frame prefix = %v, want prefix %v", types, want)
	}
	// The assistant turn must start with exactly one message_start (StreamStart
	// is idempotent), end with exactly one message_end, and have at least one
	// message_update in between (one per delta that carried real content).
	rest := types[len(want):]
	if len(rest) < 4 { // message_start, >=1 message_update, message_end, agent_end, agent_settled
		t.Fatalf("assistant-turn+tail frames = %v, too short", rest)
	}
	if rest[0] != "message_start" {
		t.Fatalf("first assistant frame = %q, want message_start", rest[0])
	}
	tail := rest[len(rest)-2:]
	if tail[0] != "agent_end" || tail[1] != "agent_settled" {
		t.Fatalf("tail frames = %v, want [agent_end agent_settled]", tail)
	}
	body := rest[1 : len(rest)-2]
	if len(body) < 2 {
		t.Fatalf("assistant turn body = %v, want >=1 message_update then message_end", body)
	}
	if body[len(body)-1] != "message_end" {
		t.Fatalf("assistant turn body = %v, want to end with message_end", body)
	}
	for _, ty := range body[:len(body)-1] {
		if ty != "message_update" {
			t.Fatalf("assistant turn body = %v, want only message_update before the final message_end", body)
		}
	}
	if n := countOfType(types, "message_start"); n != 2 { // user echo + assistant turn
		t.Fatalf("message_start emitted %d times, want 2 (user echo + one streamed turn): %v", n, types)
	}

	// The final message_end must carry the fully assembled text and priced
	// usage matching the accumulated token counts (10 in, 2 out per
	// textTurnEvents("claude-x","Hel","lo")).
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var end struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct{ Input, Output int } `json:"usage"`
		} `json:"message"`
	}
	// The assistant turn's message_end is the last frame of `body` in `rest`,
	// i.e. index len(want)+1+len(body)-1 in the full `types`/`lines` slice.
	endIdx := len(want) + 1 + len(body) - 1
	if err := json.Unmarshal([]byte(lines[endIdx]), &end); err != nil {
		t.Fatalf("unmarshal assistant message_end frame: %v", err)
	}
	if len(end.Message.Content) != 1 || end.Message.Content[0].Text != "Hello" {
		t.Fatalf("final message content = %+v, want one text block \"Hello\"", end.Message.Content)
	}
	if end.Message.Usage.Input != 10 || end.Message.Usage.Output != 2 {
		t.Fatalf("final message usage = %+v, want input=10 output=2", end.Message.Usage)
	}
}

// TestEngine_ToolUseDispatchesOnlyAfterInputFullyAccumulates proves the
// second load-bearing constraint: even though tool input arrives as
// fragmented input_json_delta events during streaming, exactly one
// tool_execution_start must fire, carrying the FULLY reassembled JSON, and
// it must fire only after the assistant turn's message_end (dispatch stays
// post-turn, via agentloop's own OnToolStart callback which only ever sees
// resp.Content after the whole turn completes).
func TestEngine_ToolUseDispatchesOnlyAfterInputFullyAccumulates(t *testing.T) {
	var gotInput json.RawMessage
	var toolCalls int
	ts := fakeToolSet{"bash": func(_ context.Context, in json.RawMessage) (string, error) {
		toolCalls++
		gotInput = in
		return "ls -la\ndone", nil
	}}
	// Fragments straight out of the cassette pattern: an initial no-op empty
	// chunk (Accumulate ignores empty PartialJSON), then "{}"-replacing first
	// real chunk, then two appended continuations.
	sender := newScriptedStreamingSender(
		streamScript{events: toolUseTurnEvents("claude-x", "tu_stream_1", "bash",
			[]string{"", `{"command":"ls `, `-la"}`})},
		streamScript{events: textTurnEvents("claude-x", "done")},
	)
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("go")
	eng.Wait()

	if toolCalls != 1 {
		t.Fatalf("tool called %d times, want exactly 1", toolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(gotInput, &args); err != nil {
		t.Fatalf("tool input %s is not valid JSON: %v", gotInput, err)
	}
	if args["command"] != "ls -la" {
		t.Fatalf("tool input = %+v, want command=%q (fully reassembled, not a partial fragment)", args, "ls -la")
	}

	types := frameTypes(t, out.String())
	startIdx, endIdx, toolIdx := -1, -1, -1
	for i, ty := range types {
		switch ty {
		case "tool_execution_start":
			if toolIdx == -1 {
				toolIdx = i
			}
		case "message_end":
			if startIdx == -1 {
				startIdx = i // first message_end is the user echo; keep scanning
			}
			endIdx = i
		}
	}
	if toolIdx == -1 {
		t.Fatalf("no tool_execution_start frame emitted: %v", types)
	}
	// The tool_execution_start must come after the assistant turn's own
	// message_end (the second message_end overall: user echo, then assistant
	// turn 1), never before it.
	var assistantEndIdx = -1
	seen := 0
	for i, ty := range types {
		if ty == "message_end" {
			seen++
			if seen == 2 {
				assistantEndIdx = i
				break
			}
		}
	}
	if assistantEndIdx == -1 {
		t.Fatalf("did not find the assistant turn's message_end: %v", types)
	}
	if toolIdx < assistantEndIdx {
		t.Fatalf("tool_execution_start at %d fired before assistant message_end at %d: %v", toolIdx, assistantEndIdx, types)
	}
	_ = endIdx
}

// TestEngine_HasContentGatePreventsOrphanedMessageStartOnPreContentFailure
// proves the reason hasContent exists: a stream that delivers only
// structural, contentless events (the SDK's own message_start, an empty
// content_block_start) and then fails must NOT leave a dangling
// message_start with no matching message_end. sendWithTrim's delivered-guard
// makes this failure unretriable (delivered was already true from the bare
// structural events), so this is exactly the "abandoned attempt" scenario
// hasContent's doc describes.
func TestEngine_HasContentGatePreventsOrphanedMessageStartOnPreContentFailure(t *testing.T) {
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{
		events:   preContentFailureEvents(),
		trailErr: errors.New("stream dropped before any content"),
	})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo only
		"agent_start",
		"agent_error",
		"agent_end", "agent_settled",
	})
	types := frameTypes(t, out.String())
	if n := countOfType(types, "message_start"); n != 1 {
		t.Fatalf("message_start emitted %d times, want exactly 1 (the user echo only): %v", n, types)
	}
}

// TestEngine_NonStreamingFallbackProducesWellFormedFrameSequence is the
// coordinator-mandated regression test: a Sender that does not implement
// llm.StreamingSender (every fake-turn child, and fakeSender specifically —
// see faketurns.go) must still produce a well-formed message_start ->
// message_update -> message_end sequence for its assistant turn, via
// OnTurn's AssistantTurn fallback branch. If OnTurn instead unconditionally
// called StreamEnd (which emits only message_end, no message_start), this
// sequence would come out malformed — see the deliberate-break proof
// recorded in task-B4-report.md.
func TestEngine_NonStreamingFallbackProducesWellFormedFrameSequence(t *testing.T) {
	ts := fakeToolSet{}
	eng, out := newTestEngine(t, ts, sampleEndTurn) // scriptedSender/fakeSender: llm.Sender only, no NewStreaming

	eng.HandlePrompt("hi")
	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn, non-streaming fallback
		"agent_end", "agent_settled",
	})
}
