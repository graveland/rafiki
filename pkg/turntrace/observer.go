// SPDX-License-Identifier: Apache-2.0

// Package turntrace builds OpenTelemetry spans from the daemon's normalized
// event stream. One trace per turn: a turn is a root span, tool calls nest
// under it, and causality between children is a span link. It is fed only from
// the daemon's single event-publish path, so every child kind produces the
// same shape.
package turntrace

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

const tracerName = "go.graveland.dev/rafiki/pkg/turntrace"

// ChildMeta is what the observer needs to know about a child when a turn
// starts. The zero value is valid; a missing child is described by
// ChildMeta{ChildID: childID}.
type ChildMeta struct {
	ChildID, Name, Kind, Model, Provider, Owner, RootID, ParentID, ConversationID string
}

// MetaFunc resolves a child's metadata. It must not call back into the
// Observer; the Observer never calls it while holding its lock.
type MetaFunc func(childID string) (ChildMeta, bool)

// Recorder receives the per-turn and per-tool summary metrics.
//
// Its methods are called with the Observer's mutex held. An implementation
// must not call back into the Observer (Observe, TurnSpanContext, ParentFor,
// LinkNext, ...) or block: taking the same mutex again is a self-deadlock, and
// a slow exporter would stall every child's event stream.
type Recorder interface {
	ObserveAgentTurn(ctx context.Context, kind, model, provider, stopReason string, d time.Duration, costUSD float64)
	ObserveTool(ctx context.Context, kind, tool string, isError bool, d time.Duration)
}

// Observer turns a child's normalized events into spans.
//
// All state is guarded by one mutex, held for the duration of a single event's
// handling. meta is always resolved before that lock is taken, so a MetaFunc
// that blocks on other daemon state can never deadlock against a concurrent
// Observe.
type Observer struct {
	tracer trace.Tracer
	policy telemetry.Policy
	meta   MetaFunc
	rec    Recorder

	mu       sync.Mutex
	children map[string]*childState
}

// New returns an Observer. A nil TracerProvider traces nothing, a nil
// MetaFunc reports every child as unknown, and a nil Recorder records no
// metrics.
func New(tp trace.TracerProvider, policy telemetry.Policy, meta MetaFunc, rec Recorder) *Observer {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return &Observer{
		tracer:   tp.Tracer(tracerName),
		policy:   policy,
		meta:     meta,
		rec:      rec,
		children: make(map[string]*childState),
	}
}

// childState is one child's in-flight trace state.
type childState struct {
	turn *turnState

	// pendingLinks belong to the child's NEXT turn; pendingReason is the first
	// link's reason, used as the next turn's trigger.
	pendingLinks  []trace.Link
	pendingReason string

	// stashedUser is a user text seen before TurnStart; it is applied to the
	// turn that opens next.
	stashedUser    string
	hasStashedUser bool

	tools       map[string]*toolState
	stashedArgs map[string]string
	// parents maps an assistant message id to the Task tool_use id that
	// spawned it.
	parents map[string]string
}

type turnState struct {
	span           trace.Span
	ctx            context.Context
	start          time.Time
	contentAllowed bool
	lastText       string
	toolCalls      int
	meta           ChildMeta
	userRecorded   bool
}

type toolState struct {
	span       trace.Span
	ctx        context.Context
	start      time.Time
	name       string
	kind       string
	endTs      time.Time
	ended      bool
	resultSeen bool
	isError    bool
}

// childLocked returns the child's state, creating it if needed. o.mu must be
// held.
func (o *Observer) childLocked(childID string) *childState {
	cs := o.children[childID]
	if cs == nil {
		cs = &childState{
			tools:       make(map[string]*toolState),
			stashedArgs: make(map[string]string),
			parents:     make(map[string]string),
		}
		o.children[childID] = cs
	}
	return cs
}

// Observe routes one event for childID. It is safe for concurrent use and a
// no-op on a nil Observer.
func (o *Observer) Observe(childID string, ev *rafikiv1.Event) {
	if o == nil || ev == nil {
		return
	}
	t := time.Now()
	if ev.GetTs() != nil {
		t = ev.GetTs().AsTime()
	}
	switch {
	case ev.GetTurnStart() != nil:
		o.observeTurnStart(childID, ev.GetTurnStart(), t)
	case ev.GetUserMessage() != nil:
		o.observeUserMessage(childID, ev.GetUserMessage(), t)
	case ev.GetAssistantMessage() != nil:
		o.observeAssistantMessage(childID, ev.GetAssistantMessage())
	case ev.GetToolExecutionStart() != nil:
		o.observeToolStart(childID, ev.GetToolExecutionStart(), t)
	case ev.GetToolExecutionEnd() != nil:
		o.observeToolEnd(childID, ev.GetToolExecutionEnd(), t)
	case ev.GetTurnEnd() != nil:
		o.observeTurnEnd(childID, ev.GetTurnEnd(), t)
	case ev.GetError() != nil:
		o.observeError(childID, ev.GetError())
	case ev.GetRetry() != nil:
		o.observeRetry(childID, ev.GetRetry(), t)
	case ev.GetCompactionBoundary() != nil:
		o.observeCompaction(childID, ev.GetCompactionBoundary(), t)
	case ev.GetChildExited() != nil:
		gone := ev.GetChildExited().GetChildId()
		if gone == "" {
			// A producer that leaves the payload id empty still names the
			// child by the envelope's routing argument; dropping the state is
			// better than leaking it for the daemon's lifetime.
			gone = childID
		}
		o.ChildGone(gone)
	}
}

func (o *Observer) observeTurnStart(childID string, start *rafikiv1.TurnStart, t time.Time) {
	meta := ChildMeta{ChildID: childID}
	if o.meta != nil {
		if m, ok := o.meta(childID); ok {
			meta = m
			if meta.ChildID == "" {
				meta.ChildID = childID
			}
		}
	}
	allowed := o.policy.Allows(meta.Owner, meta.Provider, meta.Model)

	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.childLocked(childID)
	if cs.turn != nil {
		o.endOpenSpansLocked(cs, t)
	}

	links := cs.pendingLinks
	trigger := cs.pendingReason
	cs.pendingLinks = nil
	cs.pendingReason = ""
	if trigger == "" {
		trigger = "unknown"
	}

	name := meta.Name
	if name == "" {
		name = childID
	}
	model := start.GetModel()
	if model == "" {
		model = meta.Model
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.id", childID),
		attribute.String("gen_ai.agent.name", name),
		attribute.String("gen_ai.request.model", model),
		attribute.String("enduser.id", meta.Owner),
		attribute.String("rafiki.kind", meta.Kind),
		attribute.String("rafiki.root_id", meta.RootID),
		attribute.String("rafiki.parent_id", meta.ParentID),
		attribute.String("rafiki.turn.trigger", trigger),
	}
	if meta.ConversationID != "" {
		attrs = append(attrs, attribute.String("gen_ai.conversation.id", meta.ConversationID))
	}
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithTimestamp(t),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	}
	if len(links) > 0 {
		opts = append(opts, trace.WithLinks(links...))
	}
	ctx, span := o.tracer.Start(context.Background(), "invoke_agent "+name, opts...)
	turn := &turnState{
		span:           span,
		ctx:            ctx,
		start:          t,
		contentAllowed: allowed,
		meta:           meta,
	}
	cs.turn = turn

	if cs.hasStashedUser {
		applyUserText(turn, cs.stashedUser)
		turn.userRecorded = true
		cs.stashedUser = ""
		cs.hasStashedUser = false
	}
}

func (o *Observer) observeUserMessage(childID string, msg *rafikiv1.UserMessage, t time.Time) {
	var text string
	for _, b := range msg.GetContent() {
		if tb := b.GetText(); tb != nil {
			text += tb.GetText()
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.childLocked(childID)
	if text != "" {
		switch {
		case cs.turn == nil:
			if !cs.hasStashedUser {
				cs.stashedUser = text
				cs.hasStashedUser = true
			}
		case !cs.turn.userRecorded:
			applyUserText(cs.turn, text)
			cs.turn.userRecorded = true
		}
	}

	for _, b := range msg.GetContent() {
		result := b.GetToolResult()
		if result == nil {
			continue
		}
		id := result.GetToolUseId()
		ts := cs.tools[id]
		if ts == nil {
			continue
		}
		var resultText string
		var resultBytes int
		for _, cb := range result.GetContent() {
			if tb := cb.GetText(); tb != nil {
				resultText += tb.GetText()
				resultBytes += len(tb.GetText())
			}
			if ib := cb.GetImage(); ib != nil {
				resultBytes += len(ib.GetData())
			}
		}
		ts.span.SetAttributes(attribute.Int("rafiki.tool.result.bytes", resultBytes))
		if result.GetIsError() {
			ts.span.RecordError(errors.New(telemetry.ErrorText(resultText)), trace.WithTimestamp(t))
			ts.span.SetAttributes(attribute.Int("rafiki.error.bytes", len(resultText)))
			ts.span.SetStatus(codes.Error, "")
			ts.isError = true
		}
		ts.resultSeen = true
		if ts.ended {
			o.finishToolLocked(cs, id, ts, t)
		}
	}
}

func applyUserText(turn *turnState, text string) {
	turn.span.SetAttributes(attribute.Int("rafiki.user_message.bytes", len(text)))
	if turn.contentAllowed {
		turn.span.SetAttributes(attribute.String("rafiki.user_message", telemetry.UserMessage(text)))
	}
}

func (o *Observer) observeAssistantMessage(childID string, msg *rafikiv1.AssistantMessage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.childLocked(childID)
	turn := cs.turn

	text := ""
	hasText := false
	for _, b := range msg.GetContent() {
		if tb := b.GetText(); tb != nil {
			text += tb.GetText()
			hasText = true
		}
	}
	if turn != nil && hasText {
		turn.lastText = text
	}

	for _, b := range msg.GetContent() {
		use := b.GetToolUse()
		if use == nil {
			continue
		}
		id := use.GetId()
		args := use.GetInputJson()
		if ts, ok := cs.tools[id]; ok && turn != nil {
			applyToolArgs(ts, args, turn.contentAllowed)
			continue
		}
		cs.stashedArgs[id] = args
	}
}

func applyToolArgs(ts *toolState, inputJSON string, allowed bool) {
	ts.span.SetAttributes(attribute.Int("rafiki.tool.args.bytes", len(inputJSON)))
	if !allowed {
		return
	}
	args, truncated := telemetry.ToolArgs(inputJSON)
	ts.span.SetAttributes(attribute.String("gen_ai.tool.call.arguments", args))
	if truncated {
		ts.span.SetAttributes(attribute.Bool("rafiki.tool.args_truncated", true))
	}
}

func (o *Observer) observeToolStart(childID string, start *rafikiv1.ToolExecutionStart, t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.childLocked(childID)
	turn := cs.turn
	if turn == nil {
		return
	}
	id := start.GetToolUseId()
	if cs.tools[id] != nil {
		return
	}
	parentID := start.GetParentToolUseId()
	parentCtx := turn.ctx
	thread := "main"
	if parentID != "" {
		thread = "subagent"
		if parent := cs.tools[parentID]; parent != nil {
			parentCtx = parent.ctx
		}
	}

	name := start.GetName()
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", name),
		attribute.String("gen_ai.tool.call.id", id),
		attribute.String("rafiki.thread", thread),
	}
	if parentID != "" {
		attrs = append(attrs, attribute.String("rafiki.thread.parent_tool_use_id", parentID))
	}
	ctx, span := o.tracer.Start(parentCtx, "execute_tool "+name,
		trace.WithTimestamp(t),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	ts := &toolState{span: span, ctx: ctx, start: t, name: name, kind: turn.meta.Kind}
	cs.tools[id] = ts
	if args, ok := cs.stashedArgs[id]; ok {
		applyToolArgs(ts, args, turn.contentAllowed)
		delete(cs.stashedArgs, id)
	}
	turn.toolCalls++
}

func (o *Observer) observeToolEnd(childID string, end *rafikiv1.ToolExecutionEnd, t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil {
		return
	}
	id := end.GetToolUseId()
	ts := cs.tools[id]
	if ts == nil {
		return
	}
	ts.endTs = t
	ts.ended = true
	if end.GetIsError() {
		ts.isError = true
		ts.span.SetStatus(codes.Error, "")
	}
	if ts.resultSeen {
		o.finishToolLocked(cs, id, ts, t)
	}
}

// finishToolLocked ends a tool span, records its metric and removes it. The
// span ends at its recorded endTs, or fallback when no ToolExecutionEnd was
// seen. o.mu must be held.
func (o *Observer) finishToolLocked(cs *childState, id string, ts *toolState, fallback time.Time) {
	end := ts.endTs
	if end.IsZero() {
		end = fallback
	}
	ts.span.End(trace.WithTimestamp(end))
	if o.rec != nil {
		o.rec.ObserveTool(ts.ctx, ts.kind, ts.name, ts.isError, end.Sub(ts.start))
	}
	delete(cs.tools, id)
}

// endOpenSpansLocked ends every open tool span and the open turn as
// interrupted, without touching pending links or a stashed user message (they
// belong to the next turn). o.mu must be held.
//
// The tool spans are marked interrupted too: a Task or Bash call cut off by an
// overlapping turn or a child exit must not reach the metrics as a clean
// success.
func (o *Observer) endOpenSpansLocked(cs *childState, t time.Time) {
	for id, ts := range cs.tools {
		ts.span.SetAttributes(attribute.Bool("rafiki.turn.interrupted", true))
		ts.span.SetStatus(codes.Error, "interrupted")
		ts.isError = true
		o.finishToolLocked(cs, id, ts, t)
	}
	// Arguments stashed for a tool that never started, and subagent message
	// mappings whose Task span is now gone, can never be consulted again.
	clear(cs.stashedArgs)
	clear(cs.parents)
	if cs.turn == nil {
		return
	}
	cs.turn.span.SetAttributes(attribute.Bool("rafiki.turn.interrupted", true))
	cs.turn.span.SetStatus(codes.Error, "interrupted")
	cs.turn.span.End(trace.WithTimestamp(t))
	cs.turn = nil
}

func (o *Observer) observeTurnEnd(childID string, end *rafikiv1.TurnEnd, t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil || cs.turn == nil {
		return
	}
	turn := cs.turn
	stopReason := end.GetStopReason()

	attrs := []attribute.KeyValue{
		attribute.String("rafiki.stop_reason", stopReason.String()),
		attribute.Int("rafiki.tool_calls", turn.toolCalls),
		attribute.Int("rafiki.reply.bytes", len(turn.lastText)),
	}
	if end.CostUsd != nil {
		attrs = append(attrs, attribute.Float64("rafiki.cost_usd", end.GetCostUsd()))
	}
	if turn.contentAllowed {
		attrs = append(attrs, attribute.String("rafiki.reply_snippet", telemetry.ReplySnippet(turn.lastText)))
	}
	turn.span.SetAttributes(attrs...)

	for id, ts := range cs.tools {
		o.finishToolLocked(cs, id, ts, t)
	}
	// A stashed argument for a tool that never started and a message→Task
	// mapping whose Task span is no longer open are dead once the turn ends:
	// ParentFor falls back to the turn span, which is about to close too.
	clear(cs.stashedArgs)
	clear(cs.parents)
	turn.span.End(trace.WithTimestamp(t))
	cs.turn = nil

	if o.rec != nil {
		o.rec.ObserveAgentTurn(turn.ctx, turn.meta.Kind, turn.meta.Model, turn.meta.Provider,
			stopReason.String(), t.Sub(turn.start), end.GetCostUsd())
	}
}

func (o *Observer) observeError(childID string, e *rafikiv1.ErrorEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil || cs.turn == nil {
		return
	}
	cs.turn.span.RecordError(errors.New(telemetry.ErrorText(e.GetCode() + ": " + e.GetMessage())))
	cs.turn.span.SetStatus(codes.Error, "")
}

func (o *Observer) observeRetry(childID string, retry *rafikiv1.Retry, t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil || cs.turn == nil {
		return
	}
	cs.turn.span.AddEvent("retry",
		trace.WithTimestamp(t),
		trace.WithAttributes(attribute.String("rafiki.retry.reason", retry.GetReason())),
	)
}

func (o *Observer) observeCompaction(childID string, cb *rafikiv1.CompactionBoundary, t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil || cs.turn == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("rafiki.compaction.trigger", cb.GetTrigger()),
	}
	if cb.PreTokens != nil {
		attrs = append(attrs, attribute.Int("rafiki.compaction.pre_tokens", int(cb.GetPreTokens())))
	}
	if cb.PostTokens != nil {
		attrs = append(attrs, attribute.Int("rafiki.compaction.post_tokens", int(cb.GetPostTokens())))
	}
	cs.turn.span.AddEvent("compaction", trace.WithTimestamp(t), trace.WithAttributes(attrs...))
}

// ObserveSubagent records that messageID was produced by parentToolUseID's
// Task tool call.
func (o *Observer) ObserveSubagent(childID, messageID, parentToolUseID string) {
	if o == nil || messageID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.childLocked(childID).parents[messageID] = parentToolUseID
}

// LinkNext records a span link for targetChildID's next turn.
func (o *Observer) LinkNext(targetChildID string, sc trace.SpanContext, reason string) {
	if o == nil || !sc.IsValid() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.childLocked(targetChildID)
	if len(cs.pendingLinks) == 0 {
		cs.pendingReason = reason
	}
	cs.pendingLinks = append(cs.pendingLinks, trace.Link{
		SpanContext: sc,
		Attributes:  []attribute.KeyValue{attribute.String("rafiki.link.reason", reason)},
	})
}

// TurnSpanContext returns the child's open turn's span context, or the zero
// value when no turn is open.
func (o *Observer) TurnSpanContext(childID string) trace.SpanContext {
	if o == nil {
		return trace.SpanContext{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil || cs.turn == nil {
		return trace.SpanContext{}
	}
	return cs.turn.span.SpanContext()
}

// ParentFor resolves the span a subagent message should be parented to: the
// Task tool span that produced prevMessageID when it is still open, otherwise
// the child's open turn, otherwise the zero value.
func (o *Observer) ParentFor(childID, prevMessageID string) trace.SpanContext {
	if o == nil {
		return trace.SpanContext{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil {
		return trace.SpanContext{}
	}
	if prevMessageID != "" {
		if parentID := cs.parents[prevMessageID]; parentID != "" {
			if ts := cs.tools[parentID]; ts != nil {
				return ts.span.SpanContext()
			}
		}
	}
	if cs.turn != nil {
		return cs.turn.span.SpanContext()
	}
	return trace.SpanContext{}
}

// ChildGone ends every open span for childID as interrupted and drops the
// child's state.
func (o *Observer) ChildGone(childID string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cs := o.children[childID]
	if cs == nil {
		return
	}
	o.endOpenSpansLocked(cs, time.Now())
	delete(o.children, childID)
}

// Shutdown ends every child's open spans as interrupted.
func (o *Observer) Shutdown() {
	if o == nil {
		return
	}
	o.mu.Lock()
	ids := make([]string, 0, len(o.children))
	for id := range o.children {
		ids = append(ids, id)
	}
	o.mu.Unlock()
	for _, id := range ids {
		o.ChildGone(id)
	}
}
