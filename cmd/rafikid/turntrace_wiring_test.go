// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/telemetry"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// newTraceObserver returns a tracetest-backed provider and its recorder, so a
// test can assert on the spans the observer actually built.
func newTraceObserver(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(rec)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, rec
}

func traceTurnStart() *rafikiv1.Event {
	return &rafikiv1.Event{Ts: timestamppb.Now(), Payload: &rafikiv1.Event_TurnStart{
		TurnStart: &rafikiv1.TurnStart{},
	}}
}

func traceTurnEnd() *rafikiv1.Event {
	return &rafikiv1.Event{Ts: timestamppb.Now(), Payload: &rafikiv1.Event_TurnEnd{
		TurnEnd: &rafikiv1.TurnEnd{StopReason: rafikiv1.StopReason_STOP_REASON_END_TURN},
	}}
}

func traceSpanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func traceAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// publishEvent is the ONE path a native event reaches anybody by, so the
// observer is fed there and nowhere else: publishing a turn's start and end
// through it must produce exactly the one root span, with no runtime building
// a span of its own.
func TestTurnTracePublishEventFeedsObserver(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	ctrl.st.Insert(&childstore.Session{
		ChildID:   "c_turn",
		Name:      "worker",
		Kind:      protocol.KindFundi,
		Model:     "openrouter/deepseek/deepseek-chat",
		Status:    protocol.StatusStreaming,
		StartedAt: time.Now(),
		Labels:    map[string]string{"owner": "alice", childstore.LabelRoot: "c_turn"},
	})

	ctrl.publishEvent("c_turn", traceTurnStart())
	ctrl.publishEvent("c_turn", traceTurnEnd())

	spans := rec.Ended()
	c.Require().Eq(1, len(spans), "ended spans")
	c.Eq("invoke_agent worker", spans[0].Name(), "span name")
	c.False(spans[0].Parent().IsValid(), "a turn is a root span")
	model, ok := traceAttr(spans[0], "gen_ai.request.model")
	c.True(ok, "missing gen_ai.request.model")
	c.Eq("openrouter/deepseek/deepseek-chat", model.AsString(), "gen_ai.request.model")
}

// A child a coordinator spawned inherits its cause: the parent's OPEN turn is
// linked onto the child's first turn with reason "spawn", which is also that
// turn's trigger.
func TestTurnTraceSpawnLinksParentTurn(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	parentID := spawnTestChild(t, ctrl, nil)
	ctrl.publishEvent(parentID, traceTurnStart())
	parentTurn := ctrl.trace.TurnSpanContext(parentID)
	c.Require().True(parentTurn.IsValid(), "parent turn span context")

	childID := spawnTestChildWithParent(t, ctrl, parentID)
	ctrl.publishEvent(childID, traceTurnStart())
	ctrl.publishEvent(childID, traceTurnEnd())

	// A fake-pi child has no name, so the observer falls back to the child id.
	turn := traceSpanNamed(rec.Ended(), "invoke_agent "+childID)
	c.Require().NotNil(turn, "no turn span for the spawned child")

	links := turn.Links()
	c.Require().Eq(1, len(links), "links on the child's first turn")
	c.Eq(parentTurn.SpanID(), links[0].SpanContext.SpanID(), "link target span")

	var reason string
	for _, kv := range links[0].Attributes {
		if string(kv.Key) == "rafiki.link.reason" {
			reason = kv.Value.AsString()
		}
	}
	c.Eq("spawn", reason, "rafiki.link.reason")
	trigger, ok := traceAttr(turn, "rafiki.turn.trigger")
	c.True(ok, "missing rafiki.turn.trigger")
	c.Eq("spawn", trigger.AsString(), "rafiki.turn.trigger")
}

// A message a CHILD sends through the Connect face is caused by that child's
// own open turn, so the recipient's next turn links to it with reason "send".
func TestTurnTraceAcceptLinksChildCaller(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	targetID := spawnTestChild(t, ctrl, nil)

	const callerID = "c_caller"
	ctrl.publishEvent(callerID, traceTurnStart())
	callerTurn := ctrl.trace.TurnSpanContext(callerID)
	c.Require().True(callerTurn.IsValid(), "caller turn span context")

	ctx := server.WithIdentity(t.Context(), &server.Identity{
		UserID:  "u_owner",
		Via:     server.ProvenanceChildToken,
		ChildID: callerID,
	})
	id, err := connectAccepter{c: ctrl}.Accept(ctx, inbox.Inbound{
		ChildID: targetID,
		Mode:    inbox.ModePrompt,
		Text:    "hello from the parent",
	})
	c.Require().NoError(err, "accept")
	c.False(id == "", "accepted message id")

	ctrl.publishEvent(targetID, traceTurnStart())
	ctrl.publishEvent(targetID, traceTurnEnd())

	turn := traceSpanNamed(rec.Ended(), "invoke_agent "+targetID)
	c.Require().NotNil(turn, "no turn span for the target")

	links := turn.Links()
	c.Require().Eq(1, len(links), "links on the target's next turn")
	c.Eq(callerTurn.SpanID(), links[0].SpanContext.SpanID(), "link target span")
	var reason string
	for _, kv := range links[0].Attributes {
		if string(kv.Key) == "rafiki.link.reason" {
			reason = kv.Value.AsString()
		}
	}
	c.Eq("send", reason, "rafiki.link.reason")
}

// A daemon with tracing off (no EnableTracing) still publishes events and
// spawns children: every observer method is nil-safe, so the wiring must not
// gate the data path on tracing being enabled.
func TestTurnTraceNilObserverSafe(t *testing.T) {
	ctrl := newTestController(t)

	ctrl.publishEvent("c_ghost", traceTurnStart())
	ctrl.publishEvent("c_ghost", traceTurnEnd())

	id := spawnTestChild(t, ctrl, nil)
	ctrl.publishEvent(id, traceTurnStart())
	ctrl.publishEvent(id, traceTurnEnd())

	if ctrl.trace != nil || ctrl.tracerProvider != nil {
		t.Fatalf("an untraced controller must keep both fields nil (trace=%v)", ctrl.trace)
	}
}

// The provider a span carries is the provider the child's model routes
// through: fundi's model id is provider-qualified, claude's always Anthropic.
func TestTurnTraceChildMetaProvider(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, _ := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_fundi", Kind: protocol.KindFundi, Model: "openrouter/deepseek/deepseek-chat",
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_claude", Kind: protocol.KindClaude, Model: "sonnet-latest",
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_bare", Kind: protocol.KindFundi, Model: "unqualified-model",
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	})

	meta, ok := ctrl.childMeta("c_fundi")
	c.True(ok, "fundi child meta")
	c.Eq("openrouter", meta.Provider, "fundi provider")
	c.Eq("openrouter/deepseek/deepseek-chat", meta.Model, "fundi model")

	meta, ok = ctrl.childMeta("c_claude")
	c.True(ok, "claude child meta")
	c.Eq("anthropic", meta.Provider, "claude provider")

	meta, ok = ctrl.childMeta("c_bare")
	c.True(ok, "unqualified child meta")
	c.Eq("", meta.Provider, "a model with no provider segment names none")

	_, ok = ctrl.childMeta("c_missing")
	c.False(ok, "an unknown child reports false")
}

// A script child is the easiest target that exists without a running agent:
// its delivery is deferred, so Accept succeeds without touching a process.
func insertScriptChild(ctrl *Controller, id string) {
	ctrl.st.Insert(&childstore.Session{
		ChildID: id, Kind: protocol.KindScript, Status: protocol.StatusIdle,
		StartedAt: time.Now(),
	})
}

// An abort is a lifecycle signal, not a message: it must record no link even
// when the caller's context carries a valid span and the target exists.
func TestTurnTraceAbortRecordsNoLink(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	const targetID = "c_script_abort"
	insertScriptChild(ctrl, targetID)

	// A valid operator span in the context: without the ModeAbort guard this is
	// exactly what would be linked.
	ctx, op := tp.Tracer("test").Start(t.Context(), "operator")
	defer op.End()

	id, err := connectAccepter{c: ctrl}.Accept(ctx, inbox.Inbound{
		ChildID: targetID,
		Mode:    inbox.ModeAbort,
	})
	c.Require().NoError(err, "accept an abort")
	c.False(id == "", "accepted abort id")

	ctrl.publishEvent(targetID, traceTurnStart())
	ctrl.publishEvent(targetID, traceTurnEnd())

	turn := traceSpanNamed(rec.Ended(), "invoke_agent "+targetID)
	c.Require().NotNil(turn, "no turn span for the abort target")
	c.Eq(0, len(turn.Links()), "an abort must record no link")
	trigger, ok := traceAttr(turn, "rafiki.turn.trigger")
	c.True(ok, "missing rafiki.turn.trigger")
	c.Eq("unknown", trigger.AsString(), "rafiki.turn.trigger")
}

// An operator send (nil identity) with a valid span in the request context
// links that span to the target's next turn with reason "operator".
func TestTurnTraceOperatorAcceptLinksOperatorSpan(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	const targetID = "c_script_operator"
	insertScriptChild(ctrl, targetID)

	ctx, op := tp.Tracer("test").Start(t.Context(), "operator")
	defer op.End()
	operatorSpan := op.SpanContext()

	id, err := connectAccepter{c: ctrl}.Accept(ctx, inbox.Inbound{
		ChildID: targetID,
		Mode:    inbox.ModePrompt,
		Text:    "operator prompt",
	})
	c.Require().NoError(err, "accept an operator prompt")
	c.False(id == "", "accepted prompt id")

	ctrl.publishEvent(targetID, traceTurnStart())
	ctrl.publishEvent(targetID, traceTurnEnd())

	turn := traceSpanNamed(rec.Ended(), "invoke_agent "+targetID)
	c.Require().NotNil(turn, "no turn span for the target")
	links := turn.Links()
	c.Require().Eq(1, len(links), "links on the target's next turn")
	c.Eq(operatorSpan.SpanID(), links[0].SpanContext.SpanID(), "link target span")
	var reason string
	for _, kv := range links[0].Attributes {
		if string(kv.Key) == "rafiki.link.reason" {
			reason = kv.Value.AsString()
		}
	}
	c.Eq("operator", reason, "rafiki.link.reason")
	trigger, ok := traceAttr(turn, "rafiki.turn.trigger")
	c.True(ok, "missing rafiki.turn.trigger")
	c.Eq("operator", trigger.AsString(), "rafiki.turn.trigger")
}

// A spawn that fails after its causality link was queued must leave the
// abandoned id with no observer state: a later turn for that id starts clean.
func TestTurnTraceSpawnFailureLeavesNoObserverState(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	tp, rec := newTraceObserver(t)
	ctrl.EnableTracing(tp, telemetry.Config{}.Policy(), nil)

	// A store whose initial insert fails refuses the spawn AFTER child.Spawn
	// succeeded, so the operator link was already queued. fs.failed names the
	// child id the refused spawn used.
	fs := &failingChildStore{}
	ctrl.children = fs

	ctx, op := tp.Tracer("test").Start(t.Context(), "operator")
	defer op.End()

	_, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	c.Require().Error(err, "a spawn whose initial row cannot be written must be refused")
	c.Require().Eq(1, len(fs.failed), "exactly the initial insert is attempted")
	failedID := fs.failed[0]

	if _, ok := ctrl.st.Get(failedID); ok {
		t.Fatalf("the refused spawn left %s in the in-memory store", failedID)
	}

	ctrl.publishEvent(failedID, traceTurnStart())
	ctrl.publishEvent(failedID, traceTurnEnd())

	turn := traceSpanNamed(rec.Ended(), "invoke_agent "+failedID)
	c.Require().NotNil(turn, "no turn span for the failed id")
	c.Eq(0, len(turn.Links()), "the failed spawn's link must not survive")
	trigger, ok := traceAttr(turn, "rafiki.turn.trigger")
	c.True(ok, "missing rafiki.turn.trigger")
	c.Eq("unknown", trigger.AsString(), "rafiki.turn.trigger")
}
