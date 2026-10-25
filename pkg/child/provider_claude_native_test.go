package child

import (
	"fmt"
	"math"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// nativeTypeNames reduces an event slice to its payload type names, which is
// what these tests assert on: the ORDER and SET of events is the contract the
// cockpit's session reducer depends on.
func nativeTypeNames(evs []*rafikiv1.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		switch ev.Payload.(type) {
		case *rafikiv1.Event_TurnStart:
			out = append(out, "turn_start")
		case *rafikiv1.Event_TurnEnd:
			out = append(out, "turn_end")
		case *rafikiv1.Event_AssistantMessage:
			out = append(out, "assistant_message")
		case *rafikiv1.Event_UserMessage:
			out = append(out, "user_message")
		case *rafikiv1.Event_ContentBlockDelta:
			out = append(out, "content_block_delta")
		case *rafikiv1.Event_ToolExecutionStart:
			out = append(out, "tool_execution_start")
		case *rafikiv1.Event_ToolExecutionEnd:
			out = append(out, "tool_execution_end")
		case *rafikiv1.Event_CompactionBoundary:
			out = append(out, "compaction_boundary")
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

func assertTypes(t *testing.T, got []string, want []string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.Len(got, len(want), "event types = %v, want %v", got, want)
	for i := range want {
		c.Eq(want[i], got[i], "event types = %v, want %v", got, want)
	}
}

// A text-only assistant frame with no preceding prompt echo opens its turn with
// a TurnStart and nothing else: the message itself comes from its stored row, and
// claude frames are complete messages, so no delta is synthesized either.
func TestNativeAssistantTextOpensTurnOnly(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"hello"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"turn_start"})
}

// A turn opens exactly once, at prompt delivery, and closes on the result
// frame so the NEXT turn opens a fresh one. A second prompt/steer inside the
// same turn and the first assistant frame must not produce another TurnStart:
// pkg/turntrace ends and restarts the turn span on every TurnStart, so a
// duplicate would split one turn's tool spans across two roots.
func TestNativeTurnStartOncePerTurn(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	prompt := []byte(`{"type":"prompt","message":"one"}`)
	assistant := func(text string) []byte {
		return []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"` + text + `"}]}}`)
	}

	first := p.OutboundEchoNative(prompt, 1000)
	assertTypes(t, nativeTypeNames(first), []string{"turn_start"})
	start := first[0].GetTurnStart()
	c.Require().NotNil(start, "first event is not a TurnStart")
	c.Eq("", start.GetModel(), "the prompt echo names no model")
	c.Eq(int64(1000), first[0].GetTs().AsTime().UnixMilli(), "turn_start ts is the echo's")

	// A second prompt (a steer) and the first assistant frame are mid-turn.
	steer := p.OutboundEchoNative([]byte(`{"type":"steer","message":"also"}`), 1001)
	assertTypes(t, nativeTypeNames(steer), []string{})
	firstFrame := p.BusFramesNative(assistant("one"), 1002)
	assertTypes(t, nativeTypeNames(firstFrame), []string{})

	// A second assistant frame in the same turn is mid-turn, not a new turn.
	second := p.BusFramesNative(assistant("two"), 1003)
	assertTypes(t, nativeTypeNames(second), []string{})

	// The result frame closes the turn...
	result := p.BusFramesNative([]byte(`{"type":"result","subtype":"success"}`), 1004)
	assertTypes(t, nativeTypeNames(result), []string{"turn_end"})

	// ...so the next prompt opens its own.
	next := p.OutboundEchoNative(prompt, 1005)
	assertTypes(t, nativeTypeNames(next), []string{"turn_start"})
}

// A claude turn's first chat span must nest under the turn, so the turn has to
// be open BEFORE claude's first request reaches the proxy. That request
// completes before claude emits its first assistant frame, so the TurnStart
// belongs to the prompt echo — this pins the ordering directly: the echo owns
// the only TurnStart, the assistant frame adds none, and the turn reopens after
// the result.
func TestNativeTurnStartOpensAtPromptEcho(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	prompt := []byte(`{"type":"prompt","message":"do the thing"}`)
	assistant := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"done"}]}}`)

	echo := p.OutboundEchoNative(prompt, 1000)
	assertTypes(t, nativeTypeNames(echo), []string{"turn_start"})
	c.NotNil(echo[0].GetTurnStart(), "the prompt echo must open the turn")

	frame := p.BusFramesNative(assistant, 1001)
	assertTypes(t, nativeTypeNames(frame), []string{})
	for _, ev := range frame {
		c.True(ev.GetTurnStart() == nil,
			"the first assistant frame published a second TurnStart: %v", nativeTypeNames(frame))
	}

	result := p.BusFramesNative([]byte(`{"type":"result","subtype":"success"}`), 1002)
	assertTypes(t, nativeTypeNames(result), []string{"turn_end"})

	again := p.OutboundEchoNative(prompt, 1003)
	assertTypes(t, nativeTypeNames(again), []string{"turn_start"})
}

// A frame that names no model publishes an empty one rather than a stale value:
// the model is what the OPENING frame says, and a consumer falls back to the
// child's own metadata when it is empty (pkg/turntrace does).
func TestNativeTurnStartModelComesFromTheOpeningFrame(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()

	// A system/init frame captures claude-opus-5 in provider state...
	p.BusFramesNative([]byte(`{"type":"system","subtype":"init","model":"claude-opus-5"}`), 1000)
	// ...and an assistant frame that names no model still must not inherit it:
	// the turn_start reports the frame's model, not the child's remembered one.
	evs := p.BusFramesNative([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`), 1001)

	start := evs[0].GetTurnStart()
	c.Require().NotNil(start, "first event is not a TurnStart")
	c.Eq("", start.GetModel(), "turn_start model")
}

// Two tool_result blocks in one frame each complete their own call, in block
// order. ToolUseIds must not cross.
func TestNativeUserEndsEachResultInBlockOrder(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"first output"},` +
		`{"type":"tool_result","tool_use_id":"tu_2","content":"second output","is_error":true}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "tool_execution_end"})
	want := []struct {
		id      string
		isError bool
	}{{"tu_1", false}, {"tu_2", true}}
	for i, w := range want {
		end := evs[i].GetToolExecutionEnd()
		if end == nil || end.GetToolUseId() != w.id || end.GetIsError() != w.isError {
			t.Fatalf("event %d: end = %+v, want tool_use_id=%q is_error=%v", i, evs[i], w.id, w.isError)
		}
	}
}

// Guard the whole rule in one place: the native claude vocabulary is fundi's
// vocabulary. Nothing in it may be a ContentBlockDelta.
//
// TurnStart was pinned out of this test when claude's vocabulary matched fundi's
// only by NOT emitting it, which in turn was only because the shared turnActive
// flag made the native guard unreachable. Both kinds publish a TurnStart now, so
// the exclusion moved to TestNativeTurnStartOncePerTurn, which pins the count
// and the model instead of the absence. The delta half stays: a synthesized
// delta would still duplicate the frame's complete message and, on a later
// turn, append its text to the PREVIOUS turn's finalized block.
func TestNativeVocabularyExcludesContentBlockDelta(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"a"}]}}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"thinking","thinking":"t"}]}}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{}}]}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`),
	}
	p := newClaudeProvider()
	for _, line := range lines {
		for _, name := range nativeTypeNames(p.BusFramesNative(line, 1000)) {
			assert.NewAborting(t).NotEq("content_block_delta", name, "native path emitted a delta; claude frames are complete messages")
		}
	}
}

// The prompt echo opens the turn before claude's first request reaches the
// proxy and publishes nothing else: the user's message arrives from its stored
// row.
func TestOutboundEchoNativeOpensTurnOnly(t *testing.T) {
	p := newClaudeProvider()
	frame := []byte(`{"type":"prompt","message":"do the thing"}`)

	evs := p.OutboundEchoNative(frame, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"turn_start"})
}

// Frames carrying no user-authored text produce nothing. claudeUserEcho already
// owns this judgment (it rejects abort, set_session_name and empty messages);
// re-deciding it here would be a second copy that drifts.
func TestOutboundEchoNativeIgnoresNonPromptFrames(t *testing.T) {
	p := newClaudeProvider()
	for _, frame := range []string{
		`{"type":"abort"}`,
		`{"type":"set_session_name","name":"x"}`,
		`{"type":"prompt","message":""}`,
		`not json at all`,
	} {
		evs := p.OutboundEchoNative([]byte(frame), 1000)
		assert.NewAborting(t).Empty(evs, "frame %s produced %d events, want 0", frame, len(evs))
	}
}

// OutboundEchoNative type-asserts Content to string and drops the echo when it
// is not one. This pins the invariant that assertion rests on.
//
// A test asserting the assertion's own fallback would be vacuous — nothing
// reachable through claudeUserEcho produces a non-string today. This instead
// fails the moment that stops being true, and says what to go fix: otherwise
// the change lands and every claude prompt silently stops reaching the cockpit.
func TestClaudeUserEchoContentIsAString(t *testing.T) {
	msg, _, ok := claudeUserEcho([]byte(`{"type":"prompt","message":"hi"}`), 1000)
	assert.NewAborting(t).True(ok, "claudeUserEcho rejected a valid prompt frame")
	if _, isString := msg.Content.(string); !isString {
		t.Fatalf("PiUserMessage.Content is %T, want string — OutboundEchoNative's "+
			"type assertion now drops every prompt silently; teach it the new shape",
			msg.Content)
	}
}

// Claude Code's own compaction (auto or /compact) announces itself on stdout
// with a system/compact_boundary frame. BusFramesNative is the durable event
// path, so this frame is the only live signal rafiki gets that the context was
// rewritten; without the branch the boundary is visible only on reattach.
func TestBusFramesNativeEmitsCompactionBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":182000,"post_tokens":45000}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"compaction_boundary"})
	cb := evs[0].GetCompactionBoundary()
	c.NotNil(cb, "event is not a CompactionBoundary")
	c.Eq("auto", cb.GetTrigger(), "trigger")
	// GetPreTokens/GetPostTokens return 0 on a nil pointer, so these assert
	// both presence and value — the pointers must be set, not bare zeroes.
	c.Eq(182000, cb.GetPreTokens(), "pre_tokens")
	c.Eq(45000, cb.GetPostTokens(), "post_tokens")
	c.Eq(int64(1000), evs[0].GetTs().AsTime().UnixMilli(), "ts")
}

// microcompact_boundary is a different subtype string entirely: it matches
// neither the init branch nor the compact_boundary branch and falls through to
// the default nil. No dedicated case is added for it.
func TestBusFramesNativeIgnoresMicrocompactBoundary(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"microcompact_boundary"}`)

	evs := p.BusFramesNative(line, 1000)
	assert.NewAborting(t).Empty(evs, "microcompact_boundary produced %d events, want 0", len(evs))
}

// The system/init frame carries the model into provider state and emits no
// events. Pin both halves: the compact_boundary branch must never turn an init
// frame into an event, and the early return must not have broken the model
// capture.
func TestBusFramesNativeSystemInitEmitsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5","cwd":"/tmp"}`)

	evs := p.BusFramesNative(line, 1000)
	c.Empty(evs, "system/init produced %d events, want 0", len(evs))
	c.Eq("claude-opus-5", p.st.model, "st.model")
}

// resultCosts feeds result frames carrying the given total_cost_usd values to a
// fresh provider and returns the cost each emitted turn_end reports.
func resultCosts(t *testing.T, totals ...float64) []float64 {
	t.Helper()
	p := newClaudeProvider()
	out := make([]float64, 0, len(totals))
	for i, total := range totals {
		line := []byte(fmt.Sprintf(`{"type":"result","subtype":"success","total_cost_usd":%v}`, total))
		for _, ev := range p.BusFramesNative(line, int64(1000+i)) {
			if te := ev.GetTurnEnd(); te != nil {
				out = append(out, te.GetCostUsd())
			}
		}
	}
	return out
}

// claude's stream-json result restates the SESSION's running total on every
// turn, while TurnEnd.cost_usd is one turn's cost and consumers sum it. Passing
// the total through inflated the bill quadratically.
func TestNativeTurnEndCostIsThisTurnsShareOfTheSessionTotal(t *testing.T) {
	ck := assert.NewCollecting(t)

	got := resultCosts(t, 1.0, 2.5, 2.75)

	ck.EqDeep([]float64{1.0, 1.5, 0.25}, got, "per-turn costs")
}

func TestNativeTurnEndCostsSumToTheSessionTotal(t *testing.T) {
	var sum float64
	for _, c := range resultCosts(t, 0.4, 1.1, 1.1, 3.0) {
		sum += c
	}

	assert.NewCollecting(t).Less(1e-9, math.Abs(sum-3.0), "summed turn costs must equal the final session total, got %v", sum)
}

// A total that goes DOWN means claude's counter restarted (a new process, or a
// reset the provider was not told about): the new figure is a fresh baseline,
// not a negative cost.
func TestNativeTurnEndCostRestartsWhenTheTotalDrops(t *testing.T) {
	got := resultCosts(t, 3.0, 0.4, 1.0)

	assert.NewCollecting(t).EqDeep([]float64{3.0, 0.4, 0.6}, got, "per-turn costs")
}

// A result with no total reports no cost at all, and must not disturb the
// baseline the next priced turn is measured against.
func TestNativeTurnEndWithoutACostKeepsTheBaseline(t *testing.T) {
	ck := assert.NewCollecting(t)
	p := newClaudeProvider()

	p.BusFramesNative([]byte(`{"type":"result","subtype":"success","total_cost_usd":1.0}`), 1000)
	free := p.BusFramesNative([]byte(`{"type":"result","subtype":"success"}`), 1001)
	next := p.BusFramesNative([]byte(`{"type":"result","subtype":"success","total_cost_usd":1.5}`), 1002)

	ck.Require().Len(free, 1, "events")
	ck.Nil(free[0].GetTurnEnd().CostUsd, "an unpriced result reports no cost")
	ck.Require().Len(next, 1, "events")
	ck.Eq(0.5, next[0].GetTurnEnd().GetCostUsd(), "cost measured from the last priced total")
}

// Message events come from persisted rows, so no stdout or echo translation may
// produce an AssistantMessage or UserMessage.
func TestNativeEmitsNoMessageEvents(t *testing.T) {
	cases := []struct {
		name string
		run  func(p *claudeProvider) []*rafikiv1.Event
	}{
		{"prompt echo", func(p *claudeProvider) []*rafikiv1.Event {
			return p.OutboundEchoNative([]byte(`{"type":"prompt","message":"hi"}`), 1000)
		}},
		{"assistant text", func(p *claudeProvider) []*rafikiv1.Event {
			return p.BusFramesNative([]byte(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hello"}]}}`), 1000)
		}},
		{"assistant tool_use", func(p *claudeProvider) []*rafikiv1.Event {
			return p.BusFramesNative([]byte(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{}}]}}`), 1000)
		}},
		{"user tool_result", func(p *claudeProvider) []*rafikiv1.Event {
			return p.BusFramesNative([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`), 1000)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range nativeTypeNames(tc.run(newClaudeProvider())) {
				assert.NewAborting(t).NotEq("assistant_message", name, "translator emitted a message event")
				assert.NewAborting(t).NotEq("user_message", name, "translator emitted a message event")
			}
		})
	}
}

// A Task subagent's messages are published on its own thread child from its
// rows; its frames produce nothing on the parent.
func TestNativeIgnoresSubagentFrames(t *testing.T) {
	p := newClaudeProvider()
	for _, line := range []string{
		`{"type":"assistant","parent_tool_use_id":"toolu_task1","message":{"id":"msg_s","content":[{"type":"tool_use","id":"tu_s","name":"bash","input":{}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_task1","message":{"id":"msg_s","content":[{"type":"text","text":"sub"}]}}`,
		`{"type":"user","parent_tool_use_id":"toolu_task1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_s","content":"ok"}]}}`,
	} {
		evs := p.BusFramesNative([]byte(line), 1000)
		assert.NewAborting(t).Empty(evs, "subagent frame %s produced %v", line, nativeTypeNames(evs))
	}
}

func TestGatedMessageID(t *testing.T) {
	p := newClaudeProvider()
	cases := []struct {
		name string
		line string
		want string
	}{
		{"main assistant", `{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"x"}]}}`, "msg_1"},
		{"subagent assistant", `{"type":"assistant","parent_tool_use_id":"toolu_t","message":{"id":"msg_s","content":[]}}`, ""},
		{"assistant without message", `{"type":"assistant"}`, ""},
		{"user", `{"type":"user","message":{"id":"msg_u","role":"user","content":[]}}`, ""},
		{"malformed", `not json`, ""},
		{"synthetic api error", `{"type":"assistant","isApiErrorMessage":true,"message":{"id":"7f3c2a10-0000-4000-8000-000000000000","model":"<synthetic>","content":[{"type":"text","text":"API Error"}]}}`, ""},
	}
	for _, tc := range cases {
		assert.NewAborting(t).Eq(tc.want, p.GatedMessageID([]byte(tc.line)), tc.name)
	}
}

func TestNativeToolEventsStillEmitted(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()

	evs := p.BusFramesNative([]byte(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{}}]}}`), 1000)
	assertTypes(t, nativeTypeNames(evs), []string{"turn_start", "tool_execution_start"})
	start := evs[1].GetToolExecutionStart()
	c.Eq("tu_1", start.GetToolUseId(), "tool_use_id")
	c.Eq("bash", start.GetName(), "name")

	evs = p.BusFramesNative([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"boom","is_error":true}]}}`), 1001)
	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end"})
	end := evs[0].GetToolExecutionEnd()
	c.Eq("tu_1", end.GetToolUseId(), "tool_use_id")
	c.True(end.GetIsError(), "is_error")
}
