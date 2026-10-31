package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// ---- streaming event fixtures ----
//
// These mirror rafiki's own (package-private) llm/conversation_stream_test.go
// fixtures, rebuilt here against the exported llm.StreamingSender interface
// so the engine's streaming wiring (Task B4) can be driven end to end with no
// network. The tool_use case's input_json_delta fragmentation (including the
// "{}"-replaced-not-appended first chunk) matches what the real API sends.

func streamMessageStart(model string) msg.StreamEvent {
	return msg.MessageStart{ID: "msg_stream", Model: model,
		Usage: msg.Usage{Input: msg.Ptr(10), Output: msg.Ptr(0), CacheRead: msg.Ptr(0), CacheWrite: msg.Ptr(0)}}
}

func streamTextBlockStart(index int) msg.StreamEvent {
	return msg.BlockStart{Index: index, Block: msg.Text{}}
}

func streamTextDelta(index int, text string) msg.StreamEvent {
	return msg.BlockDelta{Index: index, Kind: msg.DeltaText, Text: text}
}

func streamToolUseStart(index int, id, name string) msg.StreamEvent {
	return msg.BlockStart{Index: index, Block: msg.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}}
}

func streamInputJSONDelta(index int, partial string) msg.StreamEvent {
	return msg.BlockDelta{Index: index, Kind: msg.DeltaInputJSON, Text: partial}
}

func streamBlockStop(index int) msg.StreamEvent {
	return msg.BlockStop{Index: index}
}

func streamThinkingBlockStart(index int) msg.StreamEvent {
	return msg.BlockStart{Index: index, Block: msg.Thinking{Sig: msg.Signature{Dialect: msg.DialectAnthropic}}}
}

func streamThinkingDelta(index int, thinking string) msg.StreamEvent {
	return msg.BlockDelta{Index: index, Kind: msg.DeltaThinking, Text: thinking}
}

func streamMessageDelta(stopReason string, outputTokens int) msg.StreamEvent {
	return msg.MessageDelta{StopReason: msg.ParseStopReason(stopReason), Usage: msg.Usage{Output: msg.Ptr(int64(outputTokens))}}
}

// textTurnEvents scripts a complete single-text-block streamed turn ending in
// end_turn, split across the given deltas.
func textTurnEvents(model string, parts ...string) []msg.StreamEvent {
	ev := []msg.StreamEvent{streamMessageStart(model), streamTextBlockStart(0)}
	for _, p := range parts {
		ev = append(ev, streamTextDelta(0, p))
	}
	return append(ev, streamBlockStop(0), streamMessageDelta("end_turn", len(parts)))
}

// toolUseTurnEvents scripts a streamed turn whose only content is a single
// tool_use block, its input arriving as jsonParts input_json_delta fragments —
// the shape needed to prove tool dispatch waits for the fully reassembled JSON.
func toolUseTurnEvents(model, toolID, toolName string, jsonParts []string) []msg.StreamEvent {
	ev := []msg.StreamEvent{streamMessageStart(model), streamToolUseStart(0, toolID, toolName)}
	for _, p := range jsonParts {
		ev = append(ev, streamInputJSONDelta(0, p))
	}
	return append(ev, streamBlockStop(0), streamMessageDelta("tool_use", 10))
}

// thinkingTurnEvents scripts a complete thinking-only streamed turn ending in
// end_turn, split across the given deltas — the shape needed to prove thinking
// blocks trigger live message_update emission via hasContent.
func thinkingTurnEvents(model, thinking string, outputTokens int) []msg.StreamEvent {
	parts := splitThinking(thinking)
	ev := []msg.StreamEvent{streamMessageStart(model), streamThinkingBlockStart(0)}
	for _, p := range parts {
		ev = append(ev, streamThinkingDelta(0, p))
	}
	return append(ev, streamBlockStop(0), streamMessageDelta("end_turn", outputTokens))
}

// splitThinking splits a thinking string into chunks to simulate deltas.
func splitThinking(s string) []string {
	if s == "" {
		return nil
	}
	const chunk = 10
	var parts []string
	for i := 0; i < len(s); i += chunk {
		end := i + chunk
		if end > len(s) {
			end = len(s)
		}
		parts = append(parts, s[i:end])
	}
	return parts
}

// preContentFailureEvents scripts the stream's own structural events
// (message_start, an empty content_block_start) with NO content ever delivered
// — modeling a mid-stream failure that happens before any real content
// arrives. hasContent must gate the handler so this never fires StreamStart.
func preContentFailureEvents() []msg.StreamEvent {
	return []msg.StreamEvent{streamMessageStart("claude-x"), streamTextBlockStart(0)}
}

// fakeStreamDecoder replays a fixed event slice, then (once exhausted)
// surfaces trailErr.
type fakeStreamDecoder struct {
	events   []msg.StreamEvent
	idx      int
	trailErr error
}

func (d *fakeStreamDecoder) Next() (msg.StreamEvent, error) {
	if d.idx >= len(d.events) {
		if d.trailErr != nil {
			return nil, d.trailErr
		}
		return nil, io.EOF
	}
	ev := d.events[d.idx]
	d.idx++
	return ev, nil
}

func (d *fakeStreamDecoder) Close() error { return nil }

// streamScript is one scripted Stream call.
type streamScript struct {
	events   []msg.StreamEvent
	trailErr error
}

// scriptedStreamingSender replays queued streamScripts in order — one per
// agentloop.drive iteration. Send must never be called: every conv.Continue in
// the engine always carries llm.WithStreamHandler, so sendAttempt always tries
// the streaming path first; a call to Send is therefore a test bug (streaming
// silently fell back), not a legitimate path.
type scriptedStreamingSender struct {
	mu      sync.Mutex
	scripts []streamScript
	calls   int
}

func newScriptedStreamingSender(scripts ...streamScript) *scriptedStreamingSender {
	return &scriptedStreamingSender{scripts: scripts}
}

func (s *scriptedStreamingSender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	return nil, errors.New("scriptedStreamingSender: Send called; want Stream (a stream handler is always set)")
}

func (s *scriptedStreamingSender) Stream(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.scripts) {
		return nil, errors.New("scriptedStreamingSender: streaming scripts exhausted")
	}
	sc := s.scripts[s.calls]
	s.calls++
	return &fakeStreamDecoder{events: sc.events, trailErr: sc.trailErr}, nil
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
	c := assert.NewAborting(t)
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
	c.GreaterOrEqual(4, len(rest), "assistant-turn+tail frames = %v, too short", rest) // message_start, >=1 message_update, message_end, agent_end, agent_settled
	c.Eq("message_start", rest[0], "first assistant frame")
	tail := rest[len(rest)-2:]
	c.False(tail[0] != "agent_end" || tail[1] != "agent_settled", "tail frames = %v, want [agent_end agent_settled]", tail)
	body := rest[1 : len(rest)-2]
	c.GreaterOrEqual(2, len(body), "assistant turn body = %v, want >=1 message_update then message_end", body)
	c.Eq("message_end", body[len(body)-1], "assistant turn body = %v, want to end with message_end", body)
	for _, ty := range body[:len(body)-1] {
		c.Eq("message_update", ty, "assistant turn body = %v, want only message_update before the final message_end", body)
	}
	n := countOfType(types, "message_start")
	c.Eq(2, n, "message_start emitted %d times, want 2 (user echo + one streamed turn): %v", n, types) // user echo + assistant turn

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
	c.NoError(json.Unmarshal([]byte(lines[endIdx]), &end), "unmarshal assistant message_end frame")
	c.False(len(end.Message.Content) != 1 || end.Message.Content[0].Text != "Hello", "final message content = %+v, want one text block \"Hello\"", end.Message.Content)
	c.False(end.Message.Usage.Input != 10 || end.Message.Usage.Output != 2, "final message usage = %+v, want input=10 output=2", end.Message.Usage)
}

// TestEngine_ToolUseDispatchesOnlyAfterInputFullyAccumulates proves the
// second load-bearing constraint: even though tool input arrives as
// fragmented input_json_delta events during streaming, exactly one
// tool_execution_start must fire, carrying the FULLY reassembled JSON, and
// it must fire only after the assistant turn's message_end (dispatch stays
// post-turn, via agentloop's own OnToolStart callback which only ever sees
// resp.Content after the whole turn completes).
func TestEngine_ToolUseDispatchesOnlyAfterInputFullyAccumulates(t *testing.T) {
	c := assert.NewAborting(t)
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

	c.Eq(1, toolCalls, "tool called")
	var args map[string]string
	c.NoError(json.Unmarshal(gotInput, &args), "tool input %s is not valid JSON", gotInput)
	c.Eq("ls -la", args["command"], "tool input = %+v, want command=%q (fully reassembled, not a partial fragment)", args, "ls -la")

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
	c.NotEq(-1, toolIdx, "no tool_execution_start frame emitted: %v", types)
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
	c.NotEq(-1, assistantEndIdx, "did not find the assistant turn's message_end: %v", types)
	c.GreaterOrEqual(assistantEndIdx, toolIdx, "tool_execution_start at %d fired before assistant message_end at %d: %v", toolIdx, assistantEndIdx, types)
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
	n := countOfType(types, "message_start")
	assert.NewAborting(t).Eq(1, n, "message_start emitted %d times, want exactly 1 (the user echo only): %v", n, types)
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
		"message_start", "message_update", "message_update", "message_end", // assistant turn, non-streaming fallback
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
	prefix  []msg.StreamEvent
	suffix  []msg.StreamEvent
	release <-chan struct{}

	idx     int
	blocked bool
}

func (d *blockingStreamDecoder) Next() (msg.StreamEvent, error) {
	if d.idx < len(d.prefix) {
		d.idx++
		return d.prefix[d.idx-1], nil
	}
	if !d.blocked {
		d.blocked = true
		<-d.release
	}
	si := d.idx - len(d.prefix)
	if si >= len(d.suffix) {
		return nil, io.EOF
	}
	d.idx++
	return d.suffix[si], nil
}

func (d *blockingStreamDecoder) Close() error { return nil }

// blockingStreamingSender issues exactly one Stream call, backed by a
// blockingStreamDecoder. Send must never be called — see
// scriptedStreamingSender's doc for why that's a hard test bug, not a fallback
// path, given a stream handler is always set.
type blockingStreamingSender struct {
	prefix  []msg.StreamEvent
	suffix  []msg.StreamEvent
	release <-chan struct{}
}

func (s *blockingStreamingSender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	return nil, errors.New("blockingStreamingSender: Send called; want Stream (a stream handler is always set)")
}

func (s *blockingStreamingSender) Stream(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	return &blockingStreamDecoder{prefix: s.prefix, suffix: s.suffix, release: s.release}, nil
}

// streamStage is one phase of a stagedStreamDecoder's replay: play events,
// then (if release is non-nil) block until it closes before starting the next
// stage. Generalizes blockingStreamDecoder's single prefix/release/suffix shape
// to N stages, needed by a test that must control TWO separate flush windows
// rather than one.
type streamStage struct {
	events  []msg.StreamEvent
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
}

func (d *stagedStreamDecoder) Next() (msg.StreamEvent, error) {
	for {
		if d.sIdx >= len(d.stages) {
			return nil, io.EOF
		}
		cur := d.stages[d.sIdx]
		if d.eIdx < len(cur.events) {
			ev := cur.events[d.eIdx]
			d.eIdx++
			return ev, nil
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

func (d *stagedStreamDecoder) Close() error { return nil }

// stagedStreamingSender is blockingStreamingSender's N-gate sibling: exactly
// one Stream call, backed by a stagedStreamDecoder. Send must never be called,
// for the same reason blockingStreamingSender's is never called.
type stagedStreamingSender struct {
	stages []streamStage
}

func (s *stagedStreamingSender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	return nil, errors.New("stagedStreamingSender: Send called; want Stream (a stream handler is always set)")
}

func (s *stagedStreamingSender) Stream(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	return &stagedStreamDecoder{stages: s.stages}, nil
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
	c := assert.NewAborting(t)
	release := make(chan struct{})
	sender := &blockingStreamingSender{
		prefix: []msg.StreamEvent{
			streamMessageStart("claude-x"),
			streamTextBlockStart(0),
			streamTextDelta(0, "Hel"), // must surface to the frontend before we ever unblock
		},
		suffix: []msg.StreamEvent{
			streamTextDelta(0, "lo"),
			streamBlockStop(0),
			streamMessageDelta("end_turn", 2),
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
	n := countOfType(frameTypes(t, out.String()), "agent_end")
	c.Eq(0, n, "agent_end already emitted (%d) after only a message_update was observed and before the stream "+
		"was released — deltas are being batched and flushed at turn end, not streamed progressively as they arrive", n)

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
	c.GreaterOrEqual(3, len(rest), "assistant-turn+tail frames = %v, too short", rest) // >=1 message_update, message_end, agent_end, agent_settled
	tail := rest[len(rest)-3:]
	c.False(tail[0] != "message_end" || tail[1] != "agent_end" || tail[2] != "agent_settled", "tail frames = %v, want [message_end agent_end agent_settled]", tail)
	body := rest[:len(rest)-3]
	c.GreaterOrEqual(1, len(body), "assistant turn body = %v, want >=1 message_update", body)
	for _, ty := range body {
		c.Eq("message_update", ty, "assistant turn body = %v, want only message_update before the final message_end", body)
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
	c := assert.NewAborting(t)
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	sender := &stagedStreamingSender{stages: []streamStage{
		{
			events: []msg.StreamEvent{
				streamMessageStart("claude-x"),
				streamTextBlockStart(0),
				streamTextDelta(0, "Hel"), // triggers the turn's first (immediate) flush
			},
			release: release1,
		},
		{
			events:  []msg.StreamEvent{streamTextDelta(0, "lo")},
			release: release2,
		},
		{
			events: []msg.StreamEvent{
				streamTextDelta(0, " world"),
				streamBlockStop(0),
				streamMessageDelta("end_turn", 3),
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
	c.False(len(texts) != 1 || texts[0] != "Hel", "after gate 1, message_update texts = %q, want exactly [\"Hel\"]", texts)

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
	c.Len(texts, 2, "after gate 2 opened, message_update texts")
	c.False(texts[0] != "Hel" || texts[1] != "Hello", "message_update texts = %q, want [\"Hel\" \"Hello\"]", texts)
	c.Greater(len(texts[0]), len(texts[1]), "second update %q is not strictly longer than the first %q", texts[1], texts[0])

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
	c.Eq(final, lastAssistantText(t, out.String()), "final assistant text")
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
		assert.NewAborting(t).NoError(json.Unmarshal([]byte(lines[i]), &f), "bad frame %q", lines[i])
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

	assert.NewAborting(t).Eq("Hello", lastAssistantText(t, out.String()), "final assistant text")
}

// TestEngine_CoalescesManyDeltasIntoFewFrames proves the actual point of
// coalescing: 200 single-character deltas (arriving with no artificial
// delay, so all inside one streamFlushInterval window) must NOT produce 200
// message_update frames. The threshold of 10 is generous — a working
// coalescing gate produces exactly 1 in this scenario — but avoids the test
// itself becoming a timing assertion.
func TestEngine_CoalescesManyDeltasIntoFewFrames(t *testing.T) {
	ev := []msg.StreamEvent{streamMessageStart("claude-x"), streamTextBlockStart(0)}
	for i := 0; i < 200; i++ {
		ev = append(ev, streamTextDelta(0, "x"))
	}
	ev = append(ev, streamBlockStop(0), streamMessageDelta("end_turn", 200))

	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{events: ev})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	assert.NewAborting(t).LessOrEqual(10, countOfType(frameTypes(t, out.String()), "message_update"), "200 deltas produced")
}

// ---- message_update content assertion (Task F2) ----

// assistantUpdateTexts returns the concatenated text content of every
// message_update frame in out, in order — the sequence of partial-message
// snapshots an attached TUI would actually render across a streamed turn.
// Mirrors lastAssistantText's frame-walking shape, but collects every
// message_update rather than the last message_end.
func assistantUpdateTexts(t *testing.T, out string) []string {
	t.Helper()
	ck := assert.NewAborting(t)
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
		ck.NoError(json.Unmarshal([]byte(l), &typ), "bad frame %q", l)
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
		ck.NoError(json.Unmarshal([]byte(l), &f), "bad message_update frame %q", l)
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
	c := assert.NewAborting(t)
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{
		events: textTurnEvents("claude-x", "Hel", "lo", " world"),
	})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	texts := assistantUpdateTexts(t, out.String())
	c.NotEmpty(texts, "no assistant message_update frames")
	for i, s := range texts {
		c.NotEq("", s, "message_update[%d] carried no text; frames are flowing but empty — "+
			"this is batch delivery with extra frames (texts=%q)", i, texts)
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

// ---- thinking-only stream tests ----

// thinkingUpdateTexts returns the concatenated thinking content of every
// message_update frame in out, in order — the analogue of assistantUpdateTexts
// for thinking blocks rather than text.
func thinkingUpdateTexts(t *testing.T, out string) []string {
	t.Helper()
	ck := assert.NewAborting(t)
	var texts []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var typ struct {
			Type string `json:"type"`
		}
		ck.NoError(json.Unmarshal([]byte(l), &typ), "bad frame %q", l)
		if typ.Type != "message_update" {
			continue
		}
		var f struct {
			Message struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"content"`
			} `json:"message"`
		}
		ck.NoError(json.Unmarshal([]byte(l), &f), "bad message_update frame %q", l)
		var b strings.Builder
		for _, c := range f.Message.Content {
			if c.Type == "thinking" {
				b.WriteString(c.Thinking)
			}
		}
		texts = append(texts, b.String())
	}
	return texts
}

// lastAssistantThinking returns the thinking content of the LAST message_end
// frame in out — mirrors lastAssistantText but reads thinking blocks.
func lastAssistantThinking(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type     string `json:"type"`
					Thinking string `json:"thinking"`
				} `json:"content"`
			} `json:"message"`
		}
		assert.NewAborting(t).NoError(json.Unmarshal([]byte(lines[i]), &f), "bad frame %q", lines[i])
		if f.Type != "message_end" {
			continue
		}
		var b strings.Builder
		for _, c := range f.Message.Content {
			if c.Type == "thinking" {
				b.WriteString(c.Thinking)
			}
		}
		return b.String()
	}
	t.Fatalf("no message_end frame found in output: %q", out)
	return ""
}

// TestEngine_ThinkingOnlyStreamEmitsLiveUpdates proves that a stream whose
// only content is thinking blocks triggers live message_update emission
// (via hasContent) rather than waiting for OnTurn's AssistantTurn fallback.
func TestEngine_ThinkingOnlyStreamEmitsLiveUpdates(t *testing.T) {
	c := assert.NewAborting(t)
	release := make(chan struct{})
	sender := &blockingStreamingSender{
		prefix: []msg.StreamEvent{
			streamMessageStart("deepseek/deepseek-v4-pro"),
			streamThinkingBlockStart(0),
			streamThinkingDelta(0, "I should check options."),
		},
		suffix: []msg.StreamEvent{
			streamThinkingDelta(0, " 1) merge, 2) push, 3) keep."),
			streamBlockStop(0),
			streamMessageDelta("end_turn", 5),
		},
		release: release,
	}
	ts := fakeToolSet{}
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")

	// A live message_update must surface mid-stream, before the turn completes.
	waitFor(t, 5*time.Second, func() bool {
		return countOfType(frameTypes(t, out.String()), "message_update") >= 1
	})

	// The turn must still be in flight.
	c.Eq(0, countOfType(frameTypes(t, out.String()), "agent_end"), "agent_end already emitted (")

	// Live message_update must carry the partial thinking text.
	texts := thinkingUpdateTexts(t, out.String())
	c.False(len(texts) < 1 || texts[0] != "I should check options.", "mid-stream thinking update texts = %q, want [\"I should check options.\"]", texts)

	close(release)
	eng.Wait()

	// Final message_end must carry the complete thinking text.
	c.Eq("I should check options. 1) merge, 2) push, 3) keep.", lastAssistantThinking(t, out.String()), "final thinking")

	// The full frame sequence must be well-formed.
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
	tail := rest[len(rest)-2:]
	c.False(tail[0] != "agent_end" || tail[1] != "agent_settled", "tail frames = %v, want [agent_end agent_settled]", tail)
}

// TestEngine_ThinkingContentReachesFinalFrames proves that a thinking-only
// response — even a fast one that completes within one coalesce window —
// still produces the proper message_start/message_update/message_end
// sequence with thinking blocks intact in the final message.
func TestEngine_ThinkingContentReachesFinalFrames(t *testing.T) {
	ck := assert.NewAborting(t)
	ts := fakeToolSet{}
	sender := newScriptedStreamingSender(streamScript{
		events: thinkingTurnEvents("deepseek/deepseek-v4-pro", "Let me think about this...", 3),
	})
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	// Full frame sequence: user echo + assistant turn with thinking blocks.
	types := frameTypes(t, out.String())
	// With hasContent fixed, a fast thinking-only turn still gets at least one
	// message_update (from the first delta), plus StreamEnd's final pair.
	nUpdates := countOfType(types, "message_update")
	ck.NotEq(0, nUpdates, "no message_update frames emitted: %v — thinking-only response was suppressed", types)
	n := countOfType(types, "message_start")
	ck.Eq(2, n, "message_start emitted %d times, want 2 (user echo + assistant): %v", n, types)

	// The final message_end must carry the thinking block content.
	ck.Eq("Let me think about this...", lastAssistantThinking(t, out.String()), "final thinking")

	// The agent_end message array must also include the thinking blocks.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var agentEnd struct {
		Type     string            `json:"type"`
		Messages []json.RawMessage `json:"messages"`
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if err := json.Unmarshal([]byte(lines[i]), &agentEnd); err == nil && agentEnd.Type == "agent_end" {
			break
		}
	}
	ck.Eq("agent_end", agentEnd.Type, "no agent_end frame")
	var foundThinking bool
	for _, raw := range agentEnd.Messages {
		var content struct {
			Content []struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &content) == nil {
			for _, c := range content.Content {
				if c.Type == "thinking" && c.Thinking != "" {
					foundThinking = true
				}
			}
		}
	}
	ck.True(foundThinking, "thinking content missing from agent_end messages array")
}

// ---- native stream fixtures ----

// nativeStreamScript is one scripted native stream: the events, plus a pause
// to wait BEFORE delivering events[i]. The pause is what forces a later event
// into its own flush window, since the engine coalesces frames per
// streamFlushInterval.
type nativeStreamScript struct {
	events []msg.StreamEvent
	pauses map[int]time.Duration
}

// nativeStreamSender is a native llm.Sender replaying queued native streams in
// order, so a test drives the engine's streaming handler with no SDK adapter
// in between.
type nativeStreamSender struct {
	mu      sync.Mutex
	scripts []nativeStreamScript
	calls   int
}

func (s *nativeStreamSender) Stream(ctx context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.scripts) {
		return nil, errors.New("nativeStreamSender: scripts exhausted")
	}
	sc := s.scripts[s.calls]
	s.calls++
	return &pacedNativeDecoder{events: sc.events, pauses: sc.pauses}, nil
}

func (s *nativeStreamSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	dec, err := s.Stream(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dec.Close() }()
	acc := msg.NewAccumulator()
	for {
		ev, derr := dec.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			return nil, derr
		}
		if aerr := acc.Add(ev); aerr != nil {
			return nil, aerr
		}
	}
	return acc.Response(), nil
}

type pacedNativeDecoder struct {
	events []msg.StreamEvent
	pauses map[int]time.Duration
	next   int
}

func (d *pacedNativeDecoder) Next() (msg.StreamEvent, error) {
	if d.next >= len(d.events) {
		return nil, io.EOF
	}
	if p := d.pauses[d.next]; p > 0 {
		time.Sleep(p)
	}
	ev := d.events[d.next]
	d.next++
	return ev, nil
}

func (d *pacedNativeDecoder) Close() error { return nil }

// assistantUpdateToolArgs returns, in frame order, the toolCall blocks every
// message_update carries — the partial arguments an attached TUI renders.
func assistantUpdateToolArgs(t *testing.T, out string) []map[string]any {
	t.Helper()
	c := assert.NewAborting(t)
	var found []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		c.NoError(json.Unmarshal([]byte(l), &f), "bad frame %q", l)
		if f.Type != "message_update" || len(f.Message.Content) == 0 || f.Message.Content[0] != '[' {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Arguments json.RawMessage `json:"arguments"`
		}
		c.NoError(json.Unmarshal(f.Message.Content, &blocks), "bad message_update content %q", l)
		for _, b := range blocks {
			if b.Type != "toolCall" {
				continue
			}
			args := map[string]any{}
			c.NoError(json.Unmarshal(b.Arguments, &args), "toolCall arguments %s", b.Arguments)
			found = append(found, args)
		}
	}
	return found
}

// lastAssistantToolArgs returns the toolCall arguments of the LAST
// message_end frame — the finalized assistant message.
func lastAssistantToolArgs(t *testing.T, out string) map[string]any {
	t.Helper()
	c := assert.NewAborting(t)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		c.NoError(json.Unmarshal([]byte(lines[i]), &f), "bad frame %q", lines[i])
		if f.Type != "message_end" || len(f.Message.Content) == 0 || f.Message.Content[0] != '[' {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Arguments json.RawMessage `json:"arguments"`
		}
		c.NoError(json.Unmarshal(f.Message.Content, &blocks), "bad message_end content %q", lines[i])
		for _, b := range blocks {
			if b.Type != "toolCall" {
				continue
			}
			args := map[string]any{}
			c.NoError(json.Unmarshal(b.Arguments, &args), "toolCall arguments %s", b.Arguments)
			return args
		}
	}
	t.Fatalf("no toolCall block in any message_end frame: %q", out)
	return nil
}

// TestStreamHandlerGetsPartialSnapshots drives a NATIVE stream (no SDK adapter
// in between) and pins what the handler is handed as it goes: a growing text
// block, then an open tool_use block whose input is still a JSON prefix. The
// emitted frames are the same ones the SDK path produced — a text update per
// flush window, an empty-arguments toolCall while the input is incomplete, and
// the complete arguments only in the final message.
func TestStreamHandlerGetsPartialSnapshots(t *testing.T) {
	c := assert.NewAborting(t)
	var gotInput json.RawMessage
	ts := fakeToolSet{"bash": func(_ context.Context, in json.RawMessage) (string, error) {
		gotInput = in
		return "ok", nil
	}}
	sender := &nativeStreamSender{scripts: []nativeStreamScript{
		{
			events: []msg.StreamEvent{
				msg.MessageStart{ID: "msg_stream", Model: "claude-x"},
				msg.BlockStart{Index: 0, Block: msg.Text{}},
				msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "Hel"},
				msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "lo"},
				msg.BlockStop{Index: 0, Block: msg.Text{Text: "Hello"}},
				msg.BlockStart{Index: 1, Block: msg.ToolUse{ID: "tu_1", Name: "bash", Input: json.RawMessage(`{}`)}},
				msg.BlockDelta{Index: 1, Kind: msg.DeltaInputJSON, Text: `{"command":"ls`},
				msg.BlockDelta{Index: 1, Kind: msg.DeltaInputJSON, Text: ` -la"}`},
				msg.BlockStop{Index: 1},
				msg.MessageDelta{StopReason: msg.StopToolUse, Usage: msg.Usage{Output: msg.Ptr(5)}},
			},
			// Force the second text delta and the still-open tool's first input
			// fragment into their own flush windows.
			pauses: map[int]time.Duration{
				3: streamFlushInterval + 100*time.Millisecond,
				6: streamFlushInterval + 100*time.Millisecond,
			},
		},
		{
			events: []msg.StreamEvent{
				msg.MessageStart{ID: "msg_2", Model: "claude-x"},
				msg.BlockStart{Index: 0, Block: msg.Text{}},
				msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "done"},
				msg.BlockStop{Index: 0, Block: msg.Text{Text: "done"}},
				msg.MessageDelta{StopReason: msg.StopEndTurn, Usage: msg.Usage{Output: msg.Ptr(2)}},
			},
		},
	}}
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("hi")
	eng.Wait()

	texts := assistantUpdateTexts(t, out.String())
	c.Require().GreaterOrEqual(2, len(texts), "message_update frames = %q, want the growing text then the open tool", texts)
	c.Eq("Hel", texts[0], "first partial text")
	c.Eq("Hello", texts[1], "second partial text")

	// The open tool's input was a JSON prefix at flush time, so its partial
	// arguments are empty — exactly the pre-native behaviour (an incomplete
	// document must never surface as _raw or as a truncated value).
	partialArgs := assistantUpdateToolArgs(t, out.String())
	c.Require().NotEmpty(partialArgs, "no toolCall block in any message_update: %q", out.String())
	c.Eq(0, len(partialArgs[0]), "partial tool arguments = %+v, want {} while the input is a prefix", partialArgs[0])

	finalArgs := lastAssistantToolArgs(t, out.String())
	c.Eq("ls -la", finalArgs["command"], "final tool arguments = %+v, want the completed input", finalArgs)

	var input map[string]any
	c.NoError(json.Unmarshal(gotInput, &input), "tool input %s is not JSON", gotInput)
	c.Eq("ls -la", input["command"], "tool received %+v, want the fully reassembled input", input)
}
