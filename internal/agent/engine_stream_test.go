package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// ---- mid-flight timing fixtures (Task B5) ----
//
// The four tests above all assert on frame TYPES and COUNTS, which are
// identical whether the engine streams deltas as they arrive or buffers the
// whole turn and flushes it in one burst right before message_end/agent_end.
// blockingStreamDecoder exists to pin down TIMING instead: it replays a
// prefix of events, then blocks (holding the conv.Continue call, and with it
// the engine's turn goroutine, hostage) until the test releases it, then
// replays the rest. That lets a test observe the frontend mid-turn and prove
// something already arrived before the turn could possibly have finished.

// blockingStreamDecoder is fakeStreamDecoder's sibling: same replay
// mechanics, but Next() blocks on release once prefix is exhausted, before
// ever starting suffix. Modeled on fakeStreamDecoder above (only the
// blocking behavior differs), so it satisfies the same ssestream.Decoder
// shape.
type blockingStreamDecoder struct {
	prefix  []ssestream.Event
	suffix  []ssestream.Event
	release <-chan struct{}

	idx     int
	blocked bool
}

func (d *blockingStreamDecoder) Next() bool {
	if d.idx < len(d.prefix) {
		d.idx++
		return true
	}
	if !d.blocked {
		d.blocked = true
		<-d.release
	}
	si := d.idx - len(d.prefix)
	if si >= len(d.suffix) {
		return false
	}
	d.idx++
	return true
}

func (d *blockingStreamDecoder) Event() ssestream.Event {
	i := d.idx - 1
	if i < len(d.prefix) {
		return d.prefix[i]
	}
	return d.suffix[i-len(d.prefix)]
}
func (d *blockingStreamDecoder) Close() error { return nil }
func (d *blockingStreamDecoder) Err() error   { return nil }

// blockingStreamingSender implements llm.StreamingSender with exactly one
// NewStreaming call, backed by a blockingStreamDecoder. New must never be
// called — see scriptedStreamingSender's doc for why that's a hard test bug,
// not a fallback path, given a stream handler is always set.
type blockingStreamingSender struct {
	prefix  []ssestream.Event
	suffix  []ssestream.Event
	release <-chan struct{}
}

func (s *blockingStreamingSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, errors.New("blockingStreamingSender: New called; want NewStreaming (a stream handler is always set)")
}

func (s *blockingStreamingSender) NewStreaming(_ context.Context, _ anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](
		&blockingStreamDecoder{prefix: s.prefix, suffix: s.suffix, release: s.release}, nil), nil
}

// streamStage is one phase of a stagedStreamDecoder's replay: play events,
// then (if release is non-nil) block until it closes before starting the
// next stage. Generalizes blockingStreamDecoder's single prefix/release/suffix
// shape to N stages, needed by a test that must control TWO separate flush
// windows rather than one.
type streamStage struct {
	events  []ssestream.Event
	release <-chan struct{}
}

// stagedStreamDecoder replays a sequence of streamStages in order, blocking
// between stages exactly like blockingStreamDecoder does once, generalized to
// however many gates the caller supplies.
type stagedStreamDecoder struct {
	stages []streamStage

	sIdx   int // stage currently being drained
	eIdx   int // next event index within that stage
	waited bool

	// curS/curE record which (stage, event) index the last successful Next()
	// returned, since by the time Event() is called sIdx/eIdx may already have
	// advanced past it (Next() eagerly advances to the next stage once the
	// current one is exhausted, to decide whether to block).
	curS, curE int
}

func (d *stagedStreamDecoder) Next() bool {
	for {
		if d.sIdx >= len(d.stages) {
			return false
		}
		cur := d.stages[d.sIdx]
		if d.eIdx < len(cur.events) {
			d.curS, d.curE = d.sIdx, d.eIdx
			d.eIdx++
			return true
		}
		if cur.release != nil && !d.waited {
			d.waited = true
			<-cur.release
		}
		d.sIdx++
		d.eIdx = 0
		d.waited = false
	}
}

func (d *stagedStreamDecoder) Event() ssestream.Event { return d.stages[d.curS].events[d.curE] }
func (d *stagedStreamDecoder) Close() error           { return nil }
func (d *stagedStreamDecoder) Err() error             { return nil }

// stagedStreamingSender is blockingStreamingSender's N-gate sibling: exactly
// one NewStreaming call, backed by a stagedStreamDecoder. New must never be
// called, for the same reason blockingStreamingSender's New never is.
type stagedStreamingSender struct {
	stages []streamStage
}

func (s *stagedStreamingSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, errors.New("stagedStreamingSender: New called; want NewStreaming (a stream handler is always set)")
}

func (s *stagedStreamingSender) NewStreaming(_ context.Context, _ anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](&stagedStreamDecoder{stages: s.stages}, nil), nil
}

// waitFor polls cond every millisecond until it reports true, failing the
// test if timeout elapses first. There is no channel that fires the instant
// a frame lands in the frontend's buffer (it's a plain mutex-guarded
// io.Writer, not a channel), so this is the deadline-bounded poll the mid-turn
// assertions below need in place of a synchronizing time.Sleep.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("condition not met before deadline")
		}
	}
}

// TestEngine_StreamsMessageUpdateBeforeTurnCompletes is the load-bearing
// streaming regression the four tests above cannot catch (see this file's
// doc comment above blockingStreamDecoder): they assert on frame types and
// counts alone, which come out identical whether the engine streams deltas
// progressively or buffers the whole response and flushes it in one burst
// right before message_end. This test instead asserts on TIMING: at least
// one message_update must reach the frontend WHILE the turn is still in
// flight — i.e. strictly before message_end/agent_end. The scripted stream
// delivers one real text delta, then blocks before its closing events, so a
// batched implementation (accumulate everything, emit only from OnTurn) has
// nothing to emit until the whole turn completes and cannot make this
// assertion pass; only genuine progressive streaming can.
func TestEngine_StreamsMessageUpdateBeforeTurnCompletes(t *testing.T) {
	release := make(chan struct{})
	sender := &blockingStreamingSender{
		prefix: []ssestream.Event{
			streamMessageStart("claude-x"),
			streamTextBlockStart(0),
			streamTextDelta(0, "Hel"), // must surface to the frontend before we ever unblock
		},
		suffix: []ssestream.Event{
			streamTextDelta(0, "lo"),
			streamBlockStop(0),
			streamMessageDelta("end_turn", 2),
			streamMessageStop(),
		},
		release: release,
	}
	ts := fakeToolSet{}
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi") // queues and returns immediately; the turn runs on Engine's own worker goroutine

	waitFor(t, 5*time.Second, func() bool {
		return countOfType(frameTypes(t, out.String()), "message_update") >= 1
	})

	// The turn must still be in flight at this point: message_end (and
	// therefore agent_end, which always follows it) cannot have been emitted
	// yet, because the scripted stream is parked inside blockingStreamDecoder
	// and conv.Continue has not returned. This is exactly the assertion a
	// batched implementation fails.
	if n := countOfType(frameTypes(t, out.String()), "agent_end"); n != 0 {
		t.Fatalf("agent_end already emitted (%d) after only a message_update was observed and before the stream "+
			"was released — deltas are being batched and flushed at turn end, not streamed progressively as they arrive", n)
	}

	close(release)
	eng.Wait()

	// Once released, the rest of the scripted turn plays out normally. Task
	// D3 coalesces message_update frames to at most one per
	// streamFlushInterval (250ms): the "Hel" delta above triggered the turn's
	// first flush (lastFlush was zero, so it flushed immediately), and in
	// this test's unblocked-real-time replay the remaining events ("lo",
	// content_block_stop, message_delta, message_stop) USUALLY land well
	// inside that same window, so USUALLY none of them flushes again — but
	// that is a real-time race, not a guarantee: if the goroutine scheduler
	// or test-machine load stalls delivery of "lo" past the 250ms boundary,
	// a second, legitimate message_update fires. An exact single-update
	// assertion here would then fail for a reason that has nothing to do
	// with the invariant this test exists to check (see
	// TestEngine_MessageUpdatesGrowAcrossFlushWindows below for the test that
	// deliberately forces and asserts on that second window instead of
	// treating it as noise). So this assertion is loosened to >=1
	// message_update plus ordering, not an exact count.
	types := frameTypes(t, out.String())
	want := []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"message_start",
	}
	if !strings.HasPrefix(strings.Join(types, ","), strings.Join(want, ",")) {
		t.Fatalf("frame prefix = %v, want prefix %v", types, want)
	}
	rest := types[len(want):]
	if len(rest) < 3 { // >=1 message_update, message_end, agent_end, agent_settled
		t.Fatalf("assistant-turn+tail frames = %v, too short", rest)
	}
	tail := rest[len(rest)-3:]
	if tail[0] != "message_end" || tail[1] != "agent_end" || tail[2] != "agent_settled" {
		t.Fatalf("tail frames = %v, want [message_end agent_end agent_settled]", tail)
	}
	body := rest[:len(rest)-3]
	if len(body) < 1 {
		t.Fatalf("assistant turn body = %v, want >=1 message_update", body)
	}
	for _, ty := range body {
		if ty != "message_update" {
			t.Fatalf("assistant turn body = %v, want only message_update before the final message_end", body)
		}
	}
}

// TestEngine_MessageUpdatesGrowAcrossFlushWindows is I4: no prior test
// asserts that message_update content GROWS across more than one frame.
// 250ms coalescing plus microsecond-fast scripted fixtures means every other
// streamed test here observes at most one real flush window, so an
// implementation that flushes once per turn and never again (the exact bug
// TestEngine_MessageUpdateCarriesPartialText's doc describes: "passed
// twelve tests, nine independent reviews, and a whole-branch review") would
// pass every one of them too, as long as that single flush carries partial
// (non-empty) text. In an attached TUI this renders as one chunk, then a
// jump straight to the full reply — the same shape as the bug Phase 0.6
// fixed (assert the terminal value, miss the intermediate behaviour).
//
// This test uses stagedStreamDecoder's two gates to force two flushes more
// than streamFlushInterval apart and asserts on the sequence directly: after
// gate 1, exactly one message_update ("Hel"); only after real time
// (streamFlushInterval, guaranteed by an explicit sleep, not a race) has
// passed AND gate 2 opens does a second, strictly longer one appear
// ("Hello"), and it must be a prefix of the eventual final text. A
// once-per-turn implementation has nothing to emit at the first waitFor and
// therefore cannot reach it before the test's deadline; an implementation
// that emits but never grows the text (e.g. re-sending the same snapshot)
// fails the strict length/prefix checks below.
func TestEngine_MessageUpdatesGrowAcrossFlushWindows(t *testing.T) {
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	sender := &stagedStreamingSender{stages: []streamStage{
		{
			events: []ssestream.Event{
				streamMessageStart("claude-x"),
				streamTextBlockStart(0),
				streamTextDelta(0, "Hel"), // triggers the turn's first (immediate) flush
			},
			release: release1,
		},
		{
			events:  []ssestream.Event{streamTextDelta(0, "lo")},
			release: release2,
		},
		{
			events: []ssestream.Event{
				streamTextDelta(0, " world"),
				streamBlockStop(0),
				streamMessageDelta("end_turn", 3),
				streamMessageStop(),
			},
		},
	}}
	ts := fakeToolSet{}
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")

	// First flush: "Hel" landed with lastFlush zero, so it flushes
	// immediately regardless of timing.
	waitFor(t, 5*time.Second, func() bool {
		return countOfType(frameTypes(t, out.String()), "message_update") >= 1
	})
	texts := assistantUpdateTexts(t, out.String())
	if len(texts) != 1 || texts[0] != "Hel" {
		t.Fatalf("after gate 1, message_update texts = %q, want exactly [\"Hel\"]", texts)
	}

	// Sleep PAST streamFlushInterval before delivering "lo", so its flush
	// check (now.Sub(lastFlush) >= streamFlushInterval) genuinely passes on
	// elapsed wall-clock time — not a race with the scheduler. This is what
	// makes the second gate's opening deterministic rather than a hope that
	// the goroutine runs fast enough within one window.
	time.Sleep(streamFlushInterval + 100*time.Millisecond)
	close(release1)

	waitFor(t, 5*time.Second, func() bool {
		return countOfType(frameTypes(t, out.String()), "message_update") >= 2
	})
	texts = assistantUpdateTexts(t, out.String())
	if len(texts) != 2 {
		t.Fatalf("after gate 2 opened, message_update texts = %q, want exactly 2", texts)
	}
	if texts[0] != "Hel" || texts[1] != "Hello" {
		t.Fatalf("message_update texts = %q, want [\"Hel\" \"Hello\"]", texts)
	}
	if len(texts[1]) <= len(texts[0]) {
		t.Fatalf("second update %q is not strictly longer than the first %q", texts[1], texts[0])
	}

	const final = "Hello world"
	for i, s := range texts {
		if !strings.HasPrefix(final, s) {
			t.Fatalf("message_update[%d] = %q is not a prefix of the final message %q", i, s, final)
		}
	}

	close(release2)
	eng.Wait()

	// The final message_end (unconditional, from StreamEnd) must still carry
	// the complete text regardless of the two mid-stream snapshots above.
	if got := lastAssistantText(t, out.String()); got != final {
		t.Fatalf("final assistant text = %q, want %q", got, final)
	}
}

// ---- coalescing tests (Task D3) ----

// lastAssistantText returns the text content of the LAST message_end frame
// in out — the assistant turn's final message (the user echo also emits a
// message_end, but it always precedes the assistant turn's in these
// single-turn tests, so "last" picks the assistant one).
func lastAssistantText(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &f); err != nil {
			t.Fatalf("bad frame %q: %v", lines[i], err)
		}
		if f.Type != "message_end" {
			continue
		}
		var b strings.Builder
		for _, c := range f.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	t.Fatalf("no message_end frame found in output: %q", out)
	return ""
}

// TestEngine_CoalescingStillDeliversFinalContent proves coalescing must not
// drop the tail: whatever the flush cadence, the final state of the message
// has to reach the wire, or the TUI renders a truncated reply. Five text
// deltas spelling "Hello" are scripted with no artificial delay, so they all
// land inside a single streamFlushInterval window — meaning at most one
// message_update fires mid-stream — but StreamEnd (from OnTurn) must still
// emit the fully accumulated text regardless.
func TestEngine_CoalescingStillDeliversFinalContent(t *testing.T) {
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{
		events: textTurnEvents("claude-x", "H", "e", "l", "l", "o"),
	})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	if got := lastAssistantText(t, out.String()); got != "Hello" {
		t.Fatalf("final assistant text = %q, want %q — coalescing dropped the tail", got, "Hello")
	}
}

// TestEngine_CoalescesManyDeltasIntoFewFrames proves the actual point of
// coalescing: 200 single-character deltas (arriving with no artificial
// delay, so all inside one streamFlushInterval window) must NOT produce 200
// message_update frames. The threshold of 10 is generous — a working
// coalescing gate produces exactly 1 in this scenario — but avoids the test
// itself becoming a timing assertion.
func TestEngine_CoalescesManyDeltasIntoFewFrames(t *testing.T) {
	ev := []ssestream.Event{streamMessageStart("claude-x"), streamTextBlockStart(0)}
	for i := 0; i < 200; i++ {
		ev = append(ev, streamTextDelta(0, "x"))
	}
	ev = append(ev, streamBlockStop(0), streamMessageDelta("end_turn", 200), streamMessageStop())

	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{events: ev})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	if n := countOfType(frameTypes(t, out.String()), "message_update"); n > 10 {
		t.Fatalf("200 deltas produced %d message_update frames; coalescing is not engaging", n)
	}
}

// ---- message_update content assertion (Task F2) ----

// assistantUpdateTexts returns the concatenated text content of every
// message_update frame in out, in order — the sequence of partial-message
// snapshots an attached TUI would actually render across a streamed turn.
// Mirrors lastAssistantText's frame-walking shape, but collects every
// message_update rather than the last message_end.
func assistantUpdateTexts(t *testing.T, out string) []string {
	t.Helper()
	var texts []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		// Type-sniff first: a user-echo message_start/message_end frame's
		// message.content is a bare string ("hi"), not an array of blocks like
		// an assistant message's, so decoding straight into the assistant shape
		// below would fail on those frames before we ever get to check Type.
		var typ struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(l), &typ); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		if typ.Type != "message_update" {
			continue
		}
		var f struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad message_update frame %q: %v", l, err)
		}
		var b strings.Builder
		for _, c := range f.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		texts = append(texts, b.String())
	}
	return texts
}

// TestEngine_MessageUpdateCarriesPartialText is the assertion whose absence
// let a completely non-streaming implementation pass twelve tests, nine
// independent reviews, and a whole-branch review: every prior content
// assertion targets message_end, which is built from the complete API
// response and therefore maps fine even when every intermediate frame is
// empty. THIS is the assertion that distinguishes streaming from batch
// delivery with extra frames.
func TestEngine_MessageUpdateCarriesPartialText(t *testing.T) {
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{
		events: textTurnEvents("claude-x", "Hel", "lo", " world"),
	})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	texts := assistantUpdateTexts(t, out.String())
	if len(texts) == 0 {
		t.Fatal("no assistant message_update frames")
	}
	for i, s := range texts {
		if s == "" {
			t.Fatalf("message_update[%d] carried no text; frames are flowing but empty — "+
				"this is batch delivery with extra frames (texts=%q)", i, texts)
		}
	}
	// Coalescing (250ms) means a fast scripted turn may produce just one
	// update — that's correct and sufficient here; the assertion is that
	// updates carry content, not that there are many. Whatever text was
	// observed mid-stream must be a genuine prefix of the eventual message,
	// since accumulation only ever appends.
	const final = "Hello world"
	if last := texts[len(texts)-1]; !strings.HasPrefix(final, last) {
		t.Errorf("last update text = %q, not a prefix of the final message %q", last, final)
	}
}
