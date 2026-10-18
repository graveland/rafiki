// SPDX-License-Identifier: Apache-2.0

package turntrace

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

var baseTime = time.Unix(1700000000, 0)

func at(sec int) time.Time { return baseTime.Add(time.Duration(sec) * time.Second) }
func tsAt(sec int) *timestamppb.Timestamp {
	return timestamppb.New(at(sec))
}

func policyAll() telemetry.Policy { return telemetry.Config{Content: "all"}.Policy() }
func policyOff() telemetry.Policy { return telemetry.Config{}.Policy() }

func metaFor(id string) (ChildMeta, bool) {
	return ChildMeta{
		ChildID:        id,
		Name:           id,
		Kind:           "claude",
		Model:          "claude-test",
		Provider:       "anthropic",
		Owner:          "alice",
		RootID:         id,
		ConversationID: "conv-" + id,
	}, true
}

func newObserver(t *testing.T, policy telemetry.Policy, meta MetaFunc, rec Recorder) (*Observer, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return New(tp, policy, meta, rec), recorder
}

func attrsOf(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func findOne(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	matches := spansNamed(spans, name)
	if len(matches) != 1 {
		t.Fatalf("found %d spans named %q, want 1", len(matches), name)
	}
	return matches[0]
}

func evTurnStart(t *timestamppb.Timestamp, model string) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_TurnStart{
		TurnStart: &rafikiv1.TurnStart{Model: model},
	}}
}

func evTurnEnd(t *timestamppb.Timestamp, stop rafikiv1.StopReason, cost *float64) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_TurnEnd{
		TurnEnd: &rafikiv1.TurnEnd{StopReason: stop, CostUsd: cost},
	}}
}

func contentText(idx int32, text string) *rafikiv1.ContentBlock {
	return &rafikiv1.ContentBlock{Index: idx, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}}}
}

func contentToolUse(idx int32, id, name, input string) *rafikiv1.ContentBlock {
	return &rafikiv1.ContentBlock{Index: idx, Block: &rafikiv1.ContentBlock_ToolUse{
		ToolUse: &rafikiv1.ToolUseBlock{Id: id, Name: name, InputJson: input},
	}}
}

func contentToolResult(idx int32, id string, content []*rafikiv1.ContentBlock, isError bool) *rafikiv1.ContentBlock {
	return &rafikiv1.ContentBlock{Index: idx, Block: &rafikiv1.ContentBlock_ToolResult{
		ToolResult: &rafikiv1.ToolResultBlock{ToolUseId: id, Content: content, IsError: isError},
	}}
}

func evUserText(t *timestamppb.Timestamp, texts ...string) *rafikiv1.Event {
	var content []*rafikiv1.ContentBlock
	for i, text := range texts {
		content = append(content, contentText(int32(i), text))
	}
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: content},
	}}
}

func evToolResult(t *timestamppb.Timestamp, id, text string, isError bool) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{
			contentToolResult(0, id, []*rafikiv1.ContentBlock{contentText(0, text)}, isError),
		}},
	}}
}

func evAssistantToolUse(t *timestamppb.Timestamp, id, name, input string) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{
			contentToolUse(0, id, name, input),
		}},
	}}
}

func evAssistantText(t *timestamppb.Timestamp, text string) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{
			contentText(0, text),
		}},
	}}
}

func evToolStart(t *timestamppb.Timestamp, id, name, parent string) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_ToolExecutionStart{
		ToolExecutionStart: &rafikiv1.ToolExecutionStart{
			ToolUseId: id, Name: name, ParentToolUseId: parent,
		},
	}}
}

func evToolEnd(t *timestamppb.Timestamp, id string, isError bool) *rafikiv1.Event {
	return &rafikiv1.Event{Ts: t, Payload: &rafikiv1.Event_ToolExecutionEnd{
		ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{ToolUseId: id, IsError: isError},
	}}
}

type countingRecorder struct {
	mu         sync.Mutex
	turns      int
	tools      int
	toolErrors int
}

func (r *countingRecorder) ObserveAgentTurn(context.Context, string, string, string, string, time.Duration, float64) {
	r.mu.Lock()
	r.turns++
	r.mu.Unlock()
}

func (r *countingRecorder) ObserveTool(_ context.Context, _ string, _ string, isError bool, _ time.Duration) {
	r.mu.Lock()
	r.tools++
	if isError {
		r.toolErrors++
	}
	r.mu.Unlock()
}

func (r *countingRecorder) counts() (turns, tools, toolErrors int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns, r.tools, r.toolErrors
}

func TestTurnIsRootWithToolChild(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Read", `{"file_path":"/x"}`))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Read", ""))
	o.Observe("c1", evToolEnd(tsAt(3), "t1", false))
	o.Observe("c1", evToolResult(tsAt(4), "t1", "0123456789", false))
	cost := 0.25
	o.Observe("c1", evTurnEnd(tsAt(5), rafikiv1.StopReason_STOP_REASON_END_TURN, &cost))

	spans := rec.Ended()
	c := assert.NewAborting(t)
	c.Eq(2, len(spans), "span count")
	turn := findOne(t, spans, "invoke_agent c1")
	c.Eq(false, turn.Parent().IsValid(), "turn must be a root span")
	c.Eq(0.25, attrsOf(turn)["rafiki.cost_usd"].AsFloat64(), "rafiki.cost_usd")
	tool := findOne(t, spans, "execute_tool Read")
	c.Eq(turn.SpanContext().SpanID(), tool.Parent().SpanID(), "tool parent is the turn")
	c.Eq(int64(10), attrsOf(tool)["rafiki.tool.result.bytes"].AsInt64(), "rafiki.tool.result.bytes")
}

func TestToolEndBeforeAndAfterResult(t *testing.T) {
	cases := []struct {
		name            string
		endAt, resultAt int
	}{
		{"end before result", 3, 4},
		{"result before end", 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &countingRecorder{}
			o, rec := newObserver(t, policyAll(), metaFor, r)
			o.Observe("c1", evTurnStart(tsAt(0), ""))
			o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", `{}`))
			o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
			o.Observe("c1", evToolEnd(tsAt(tc.endAt), "t1", false))
			o.Observe("c1", evToolResult(tsAt(tc.resultAt), "t1", "ok", false))
			o.Observe("c1", evTurnEnd(tsAt(10), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

			tool := findOne(t, rec.Ended(), "execute_tool Bash")
			c := assert.NewAborting(t)
			c.True(tool.EndTime().Equal(at(3)), "tool ends at ToolExecutionEnd's timestamp")
			_, tools, _ := r.counts()
			c.Eq(1, tools, "ObserveTool called exactly once")
		})
	}
}

func TestContentGatedByPolicy(t *testing.T) {
	const (
		userText = "hello world"
		input    = `{"file_path":"/x/y"}`
		reply    = "done"
	)
	drive := func(o *Observer) {
		o.Observe("c1", evUserText(tsAt(0), userText))
		o.Observe("c1", evTurnStart(tsAt(1), ""))
		o.Observe("c1", evAssistantToolUse(tsAt(2), "t1", "Read", input))
		o.Observe("c1", evToolStart(tsAt(3), "t1", "Read", ""))
		o.Observe("c1", evToolEnd(tsAt(4), "t1", false))
		o.Observe("c1", evToolResult(tsAt(5), "t1", "ok", false))
		o.Observe("c1", evAssistantText(tsAt(6), reply))
		o.Observe("c1", evTurnEnd(tsAt(7), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))
	}

	t.Run("denied", func(t *testing.T) {
		o, rec := newObserver(t, policyOff(), metaFor, nil)
		drive(o)
		spans := rec.Ended()
		turn := findOne(t, spans, "invoke_agent c1")
		tattrs := attrsOf(turn)
		tool := findOne(t, spans, "execute_tool Read")
		toolAttrs := attrsOf(tool)

		c := assert.NewAborting(t)
		_, ok := tattrs["rafiki.user_message"]
		c.Eq(false, ok, "rafiki.user_message must be absent")
		_, ok = tattrs["rafiki.reply_snippet"]
		c.Eq(false, ok, "rafiki.reply_snippet must be absent")
		_, ok = toolAttrs["gen_ai.tool.call.arguments"]
		c.Eq(false, ok, "gen_ai.tool.call.arguments must be absent")
		c.Eq(int64(len(userText)), tattrs["rafiki.user_message.bytes"].AsInt64(), "user message bytes")
		c.Eq(int64(len(reply)), tattrs["rafiki.reply.bytes"].AsInt64(), "reply bytes")
		c.Eq(int64(len(input)), toolAttrs["rafiki.tool.args.bytes"].AsInt64(), "args bytes")
		c.Eq(int64(2), toolAttrs["rafiki.tool.result.bytes"].AsInt64(), "result bytes")
	})

	t.Run("allowed", func(t *testing.T) {
		o, rec := newObserver(t, policyAll(), metaFor, nil)
		drive(o)
		spans := rec.Ended()
		turn := findOne(t, spans, "invoke_agent c1")
		tattrs := attrsOf(turn)
		tool := findOne(t, spans, "execute_tool Read")
		toolAttrs := attrsOf(tool)

		c := assert.NewAborting(t)
		c.Eq(userText, tattrs["rafiki.user_message"].AsString(), "rafiki.user_message")
		c.Eq(reply, tattrs["rafiki.reply_snippet"].AsString(), "rafiki.reply_snippet")
		c.Eq(input, toolAttrs["gen_ai.tool.call.arguments"].AsString(), "gen_ai.tool.call.arguments")
	})
}

func TestErrorsAlwaysRecorded(t *testing.T) {
	o, rec := newObserver(t, policyOff(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", `{"command":"false"}`))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
	o.Observe("c1", evToolResult(tsAt(3), "t1", "boom", true))
	o.Observe("c1", evToolEnd(tsAt(4), "t1", true))
	o.Observe("c1", evTurnEnd(tsAt(5), rafikiv1.StopReason_STOP_REASON_ERROR, nil))

	tool := findOne(t, rec.Ended(), "execute_tool Bash")
	found := false
	for _, e := range tool.Events() {
		if e.Name != "exception" {
			continue
		}
		for _, kv := range e.Attributes {
			if string(kv.Key) == "exception.message" && kv.Value.AsString() == "boom" {
				found = true
			}
		}
	}
	c := assert.NewAborting(t)
	c.True(found, "tool span records an exception event with message boom")
	c.Eq(codes.Error, tool.Status().Code, "tool status")
}

func TestUserMessageBeforeTurnStartIsStashed(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evUserText(tsAt(0), "hi"))
	o.Observe("c1", evTurnStart(tsAt(1), ""))
	o.Observe("c1", evTurnEnd(tsAt(2), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	turn := findOne(t, rec.Ended(), "invoke_agent c1")
	c := assert.NewAborting(t)
	c.Eq(int64(2), attrsOf(turn)["rafiki.user_message.bytes"].AsInt64(), "rafiki.user_message.bytes")
	c.Eq("hi", attrsOf(turn)["rafiki.user_message"].AsString(), "rafiki.user_message")
}

func TestSubagentToolNestsUnderTask(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "task1", "Task", `{"prompt":"go"}`))
	o.Observe("c1", evToolStart(tsAt(2), "task1", "Task", ""))
	o.Observe("c1", evAssistantToolUse(tsAt(3), "t2", "Bash", `{"command":"ls"}`))
	o.Observe("c1", evToolStart(tsAt(4), "t2", "Bash", "task1"))
	o.Observe("c1", evToolEnd(tsAt(5), "t2", false))
	o.Observe("c1", evToolResult(tsAt(6), "t2", "ok", false))
	o.Observe("c1", evToolEnd(tsAt(7), "task1", false))
	o.Observe("c1", evToolResult(tsAt(8), "task1", "done", false))
	o.Observe("c1", evTurnEnd(tsAt(9), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	spans := rec.Ended()
	task := findOne(t, spans, "execute_tool Task")
	sub := findOne(t, spans, "execute_tool Bash")
	c := assert.NewAborting(t)
	c.Eq(task.SpanContext().SpanID(), sub.Parent().SpanID(), "t2 nests under task1")
	c.Eq("subagent", attrsOf(sub)["rafiki.thread"].AsString(), "rafiki.thread")
	c.Eq("task1", attrsOf(sub)["rafiki.thread.parent_tool_use_id"].AsString(), "parent tool use id")
	c.Eq("main", attrsOf(task)["rafiki.thread"].AsString(), "task1 runs on the main thread")
}

func TestLinksAttachToNextTurn(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("A", evTurnStart(tsAt(0), ""))
	aSC := o.TurnSpanContext("A")
	o.Observe("A", evTurnEnd(tsAt(1), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	o.LinkNext("B", aSC, "spawn")
	o.Observe("B", evTurnStart(tsAt(2), ""))
	o.Observe("B", evTurnEnd(tsAt(3), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))
	o.Observe("B", evTurnStart(tsAt(4), ""))
	o.Observe("B", evTurnEnd(tsAt(5), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	turns := spansNamed(rec.Ended(), "invoke_agent B")
	c := assert.NewAborting(t)
	c.Eq(2, len(turns), "B turn spans")
	first, second := turns[0], turns[1]
	if first.StartTime().After(second.StartTime()) {
		first, second = second, first
	}
	links := first.Links()
	c.Eq(1, len(links), "first B turn has one link")
	c.Eq(aSC.SpanID(), links[0].SpanContext.SpanID(), "link points at A's turn")
	var reason string
	for _, kv := range links[0].Attributes {
		if string(kv.Key) == "rafiki.link.reason" {
			reason = kv.Value.AsString()
		}
	}
	c.Eq("spawn", reason, "rafiki.link.reason")
	c.Eq("spawn", attrsOf(first)["rafiki.turn.trigger"].AsString(), "rafiki.turn.trigger")
	c.Eq(0, len(second.Links()), "second B turn has no links")
	c.Eq("unknown", attrsOf(second)["rafiki.turn.trigger"].AsString(), "second trigger")
}

func TestParentForResolvesSubagentThread(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "task1", "Task", `{}`))
	o.Observe("c1", evToolStart(tsAt(2), "task1", "Task", ""))
	turnSC := o.TurnSpanContext("c1")
	o.ObserveSubagent("c1", "msg_1", "task1")

	known := o.ParentFor("c1", "msg_1")
	unknown := o.ParentFor("c1", "msg_never_seen")

	o.Observe("c1", evToolEnd(tsAt(3), "task1", false))
	o.Observe("c1", evToolResult(tsAt(4), "task1", "ok", false))
	o.Observe("c1", evTurnEnd(tsAt(5), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	task := findOne(t, rec.Ended(), "execute_tool Task")
	c := assert.NewAborting(t)
	c.True(known.IsValid(), "known message resolves to the open task span")
	c.Eq(task.SpanContext().SpanID(), known.SpanID(), "ParentFor returns the task span")
	c.Eq(turnSC.SpanID(), unknown.SpanID(), "unknown message falls back to the turn")
	c.Eq(false, o.ParentFor("c1", "msg_1").IsValid(), "no open turn or tool span is invalid")
	c.Eq(false, o.ParentFor("missing", "msg_1").IsValid(), "unknown child is invalid")
}

func TestChildGoneInterruptsOpenTurn(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", `{}`))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
	o.ChildGone("c1")

	spans := rec.Ended()
	c := assert.NewAborting(t)
	c.Eq(2, len(spans), "both open spans end")
	turn := findOne(t, spans, "invoke_agent c1")
	c.Eq(true, attrsOf(turn)["rafiki.turn.interrupted"].AsBool(), "rafiki.turn.interrupted")
	c.Eq(codes.Error, turn.Status().Code, "turn status")
	findOne(t, spans, "execute_tool Bash")
	c.Eq(false, o.TurnSpanContext("c1").IsValid(), "state is dropped")
}

func TestShutdownEndsEverything(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("a", evTurnStart(tsAt(0), ""))
	o.Observe("a", evToolStart(tsAt(1), "ta", "Bash", ""))
	o.Observe("b", evTurnStart(tsAt(2), ""))
	o.Observe("b", evToolStart(tsAt(3), "tb", "Read", ""))
	o.Shutdown()

	spans := rec.Ended()
	c := assert.NewAborting(t)
	c.Eq(4, len(spans), "every open span ends")
	for _, name := range []string{"invoke_agent a", "invoke_agent b"} {
		c.Eq(true, attrsOf(findOne(t, spans, name))["rafiki.turn.interrupted"].AsBool(), name)
	}
}

func TestTurnStartWhileOpenKeepsPendingLinks(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	sc := o.TurnSpanContext("c1")
	o.LinkNext("c1", sc, "spawn")
	o.Observe("c1", evTurnStart(tsAt(1), ""))
	o.Observe("c1", evTurnEnd(tsAt(2), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	turns := spansNamed(rec.Ended(), "invoke_agent c1")
	c := assert.NewAborting(t)
	c.Eq(2, len(turns), "two turn spans")
	first, second := turns[0], turns[1]
	if first.StartTime().After(second.StartTime()) {
		first, second = second, first
	}
	c.Eq(true, attrsOf(first)["rafiki.turn.interrupted"].AsBool(), "first turn is interrupted")
	c.Eq(1, len(second.Links()), "pending link survives the interrupted turn")
	c.Eq("spawn", attrsOf(second)["rafiki.turn.trigger"].AsString(), "second turn trigger")
}

func TestNilObserverIsNoop(t *testing.T) {
	var o *Observer
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.ObserveSubagent("c1", "m", "p")
	o.LinkNext("c1", trace.SpanContext{}, "reason")
	o.ChildGone("c1")
	o.Shutdown()

	c := assert.NewAborting(t)
	c.Eq(false, o.TurnSpanContext("c1").IsValid(), "TurnSpanContext")
	c.Eq(false, o.ParentFor("c1", "m").IsValid(), "ParentFor")
}

func TestConcurrentObserve(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)

	const (
		goroutines = 8
		turns      = 50
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			for turn := 0; turn < turns; turn++ {
				base := turn * 10
				tool := fmt.Sprintf("t%d", turn)
				o.Observe(id, evTurnStart(tsAt(base), ""))
				o.Observe(id, evAssistantToolUse(tsAt(base+1), tool, "Read", `{"a":1}`))
				o.Observe(id, evToolStart(tsAt(base+2), tool, "Read", ""))
				o.Observe(id, evToolEnd(tsAt(base+3), tool, false))
				o.Observe(id, evToolResult(tsAt(base+4), tool, "ok", false))
				o.Observe(id, evTurnEnd(tsAt(base+5), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))
			}
		}(i)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				o.TurnSpanContext("c0")
				o.ParentFor("c1", "msg")
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	c := assert.NewAborting(t)
	c.Eq(goroutines*turns*2, len(rec.Ended()), "span count")
}

func TestInterruptedToolSpanIsMarked(t *testing.T) {
	r := &countingRecorder{}
	o, rec := newObserver(t, policyAll(), metaFor, r)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", `{"command":"sleep 100"}`))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
	o.Observe("c1", evTurnStart(tsAt(3), ""))
	o.Observe("c1", evTurnEnd(tsAt(4), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	spans := rec.Ended()
	tool := findOne(t, spans, "execute_tool Bash")
	c := assert.NewAborting(t)
	c.Eq(true, attrsOf(tool)["rafiki.turn.interrupted"].AsBool(), "tool rafiki.turn.interrupted")
	c.Eq(codes.Error, tool.Status().Code, "tool status")
	_, _, toolErrors := r.counts()
	c.Eq(1, toolErrors, "ObserveTool saw isError=true")
}

func TestChildExitedWithoutPayloadIDDropsState(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", `{}`))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
	o.LinkNext("c1", o.TurnSpanContext("c1"), "spawn")

	// The payload names no child; the envelope's routing argument must be used.
	o.Observe("c1", &rafikiv1.Event{Payload: &rafikiv1.Event_ChildExited{
		ChildExited: &rafikiv1.ChildExited{},
	}})
	c := assert.NewAborting(t)
	c.Eq(false, o.TurnSpanContext("c1").IsValid(), "child state dropped")

	o.Observe("c1", evTurnStart(tsAt(5), ""))
	o.Observe("c1", evTurnEnd(tsAt(6), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	turns := spansNamed(rec.Ended(), "invoke_agent c1")
	c.Eq(2, len(turns), "a fresh turn opened")
	fresh, old := turns[0], turns[1]
	if fresh.StartTime().Before(old.StartTime()) {
		fresh, old = old, fresh
	}
	c.Eq(0, len(fresh.Links()), "no pending link survived")
	c.Eq("unknown", attrsOf(fresh)["rafiki.turn.trigger"].AsString(), "fresh trigger")
	c.Eq(int64(0), attrsOf(fresh)["rafiki.tool_calls"].AsInt64(), "no tool count survived")
	c.Eq(true, attrsOf(old)["rafiki.turn.interrupted"].AsBool(), "old turn was interrupted")
}

func TestToolArgsTruncated(t *testing.T) {
	o, rec := newObserver(t, policyAll(), metaFor, nil)
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":"%s"`, i, strings.Repeat("x", 200))
	}
	b.WriteString("}")
	big := b.String()
	o.Observe("c1", evTurnStart(tsAt(0), ""))
	o.Observe("c1", evAssistantToolUse(tsAt(1), "t1", "Bash", big))
	o.Observe("c1", evToolStart(tsAt(2), "t1", "Bash", ""))
	o.Observe("c1", evToolEnd(tsAt(3), "t1", false))
	o.Observe("c1", evToolResult(tsAt(4), "t1", "ok", false))
	o.Observe("c1", evTurnEnd(tsAt(5), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))

	tool := findOne(t, rec.Ended(), "execute_tool Bash")
	c := assert.NewAborting(t)
	c.Eq(true, attrsOf(tool)["rafiki.tool.args_truncated"].AsBool(), "rafiki.tool.args_truncated")
}

func TestContentGateIsPerOwner(t *testing.T) {
	meta := func(id string) (ChildMeta, bool) {
		m, _ := metaFor(id)
		if id == "bob" {
			m.Owner = "bob"
		}
		return m, true
	}
	policy := telemetry.Config{Content: "rules", Rules: []telemetry.Rule{{User: "alice"}}}.Policy()
	o, rec := newObserver(t, policy, meta, nil)
	for _, id := range []string{"alice", "bob"} {
		o.Observe(id, evUserText(tsAt(0), "secret"))
		o.Observe(id, evTurnStart(tsAt(1), ""))
		o.Observe(id, evAssistantText(tsAt(2), "reply"))
		o.Observe(id, evTurnEnd(tsAt(3), rafikiv1.StopReason_STOP_REASON_END_TURN, nil))
	}
	spans := rec.Ended()
	alice := attrsOf(findOne(t, spans, "invoke_agent alice"))
	bob := attrsOf(findOne(t, spans, "invoke_agent bob"))
	c := assert.NewAborting(t)
	c.Eq("secret", alice["rafiki.user_message"].AsString(), "alice's content is allowed")
	_, ok := bob["rafiki.user_message"]
	c.Eq(false, ok, "bob's content is denied")
	c.Eq(int64(6), bob["rafiki.user_message.bytes"].AsInt64(), "bob still gets the byte count")
}
