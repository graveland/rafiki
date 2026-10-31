// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"bytes"
	"io"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/toolmeta"
)

type nativeCapture struct{ events []*rafikiv1.Event }

func (c *nativeCapture) Publish(ev *rafikiv1.Event) { c.events = append(c.events, ev) }

// The assistant's reply is the SUBSTANCE of a transcript, and it was never
// published. publishNative carried an AssistantMessage arm that nothing ever
// called, so the durable event log held child_spawned, user_message, the tool
// events and child_exited — and nothing the model actually said. A cockpit
// attaching to any agent saw its own prompts and no answers.
func TestEmitterPublishesNativeAssistantMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.publishAssistant(&msg.Response{
		StopReason: msg.StopEndTurn,
		Content: []msg.Block{
			msg.Thinking{Text: "hmm", Sig: msg.Signature{Value: "sig-1"}},
			msg.Text{Text: "the answer"},
			msg.ToolUse{ID: "tu_1", Name: "bash", Input: []byte(`{"cmd":"ls"}`)},
		},
	}, 0, store.Appended{CreatedAt: time.Now()})

	am := findAssistant(t, sink.events)
	c.Eq("end_turn", am.GetRawStopReason(), "raw stop reason")
	c.Eq(rafikiv1.StopReason_STOP_REASON_END_TURN, am.GetStopReason(), "stop reason")
	c.Require().Len(am.GetContent(), 3, "got %d content blocks, want 3", len(am.GetContent()))
	c.Eq("hmm", am.GetContent()[0].GetThinking().GetThinking(), "thinking")
	c.Eq("the answer", am.GetContent()[1].GetText().GetText(), "text")
	tu := am.GetContent()[2].GetToolUse()
	c.False(tu.GetId() != "tu_1" || tu.GetName() != "bash" || tu.GetInputJson() != `{"cmd":"ls"}`, "tool use = %+v", tu)
}

// An unmodelled provider stop reason must reach RawStopReason verbatim: it is
// the event stream's own spelling of why the turn stopped, and erasing it to ""
// loses the one signal that explains a turn ending oddly (Anthropic's
// model_context_window_exceeded, say). A modelled value is unchanged.
func TestEmitterKeepsAnUnmodelledStopReasonVerbatim(t *testing.T) {
	c := assert.NewCollecting(t)
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), io.Discard, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.publishAssistant(&msg.Response{
		StopReasonRaw: "model_context_window_exceeded",
		Content:       []msg.Block{msg.Text{Text: "cut off"}},
	}, 0, store.Appended{CreatedAt: time.Now()})

	am := findAssistant(t, sink.events)
	c.Eq("model_context_window_exceeded", am.GetRawStopReason(), "unmodelled raw stop reason")
	c.Eq(rafikiv1.StopReason_STOP_REASON_UNSPECIFIED, am.GetStopReason(), "unmodelled stop reason")

	// A modelled value is unaffected by the fallback.
	sink.events = nil
	em.publishAssistant(&msg.Response{
		StopReason: msg.StopEndTurn,
		Content:    []msg.Block{msg.Text{Text: "done"}},
	}, 0, store.Appended{CreatedAt: time.Now()})
	am = findAssistant(t, sink.events)
	c.Eq("end_turn", am.GetRawStopReason(), "modelled raw stop reason")
	c.Eq(rafikiv1.StopReason_STOP_REASON_END_TURN, am.GetStopReason(), "modelled stop reason")
}

// The STREAMED path must publish it too, and this is the arm that actually
// matters: content_block_delta is ephemeral and never logged, so a streamed
// turn that skipped the assistant message would be lost on every replay — and
// in practice every turn streams.
func TestStreamedTurnPublishesTheAssistantMessage(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	resp := &msg.Response{
		StopReason: msg.StopEndTurn,
		Content:    []msg.Block{msg.Text{Text: "streamed reply"}},
	}
	em.StreamStart(MapAssistantMessage(resp, "anthropic", nil))
	em.StreamEnd(MapAssistantMessage(resp, "anthropic", nil))
	em.publishAssistant(resp, 0, store.Appended{CreatedAt: time.Now()})

	am := findAssistant(t, sink.events)
	assert.NewCollecting(t).Eq("streamed reply", am.GetContent()[0].GetText().GetText(), "text")
}

func TestPublishAssistantCarriesRunningCost(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.publishAssistant(&msg.Response{
		StopReason: msg.StopEndTurn,
		Content:    []msg.Block{msg.Text{Text: "hi"}},
	}, 1.2345, store.Appended{CreatedAt: time.Now()})

	am := findAssistant(t, sink.events)
	assert.NewAborting(t).NotNil(am.CostUsd, "CostUsd is nil, want the running total")
	if diff := am.GetCostUsd() - 1.2345; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUsd = %v, want 1.2345", am.GetCostUsd())
	}
}

// turn_end is durable and is how a non-TUI consumer sees a turn boundary at
// all. It carries the turn's usage so a cost consumer needs no second source.
func TestAgentEndPublishesTurnEnd(t *testing.T) {
	c := assert.NewCollecting(t)
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	// Driven exactly as the engine drives it: the pi frames fold the usage,
	// then the durable publication, then the turn ends.
	resp := &msg.Response{
		StopReason: msg.StopEndTurn,
		Content:    []msg.Block{msg.Text{Text: "done"}},
		Usage:      msg.Usage{Input: msg.Ptr(10), Output: msg.Ptr(3)},
	}
	em.AssistantTurn(resp)
	em.publishAssistant(resp, 0, store.Appended{CreatedAt: time.Now()})
	em.AgentEnd()

	var te *rafikiv1.TurnEnd
	for _, ev := range sink.events {
		if t2 := ev.GetTurnEnd(); t2 != nil {
			te = t2
		}
	}
	c.Require().NotNil(te, "no turn_end published; got %d events", len(sink.events))
	c.Eq("end_turn", te.GetRawStopReason(), "turn_end raw stop reason")
	if te.GetUsage().GetInputTokens() != 10 || te.GetUsage().GetOutputTokens() != 3 {
		t.Errorf("turn_end usage = %+v, want in=10 out=3", te.GetUsage())
	}
}

func findAssistant(t *testing.T, evs []*rafikiv1.Event) *rafikiv1.AssistantMessage {
	t.Helper()
	for _, ev := range evs {
		if am := ev.GetAssistantMessage(); am != nil {
			return am
		}
	}
	t.Fatalf("no assistant_message among %d published events", len(evs))
	return nil
}

// Live tool output rides a UserMessage carrying a tool_result block —
// ToolExecutionEnd has no room for text — and is published with the row that
// stores it (publishUserRow), never from ToolEnd, so it carries the row's ts.
func TestToolResultPublishesWithItsRow(t *testing.T) {
	c := assert.NewCollecting(t)
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.ToolStart("tu_1", "bash", []byte(`{"command":"false"}`))
	em.ToolEnd("tu_1", "bash", toolmeta.Result{Text: "cat: /nope: No such file"}, true)
	for _, ev := range sink.events {
		c.Nil(ev.GetUserMessage(), "ToolEnd published a user_message; only the row's insert may")
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	em.publishUserRow([]msg.Block{
		msg.ToolResult{
			ToolUseID: "tu_1",
			Content:   []msg.Block{msg.Text{Text: "cat: /nope: No such file"}},
			IsError:   true,
		},
	}, store.Appended{CreatedAt: at})

	ev := sink.events[len(sink.events)-1]
	c.True(ev.GetTs().AsTime().Equal(at), "ts = %v, want the row's created_at %v", ev.GetTs().AsTime(), at)
	tr := ev.GetUserMessage().GetContent()[0].GetToolResult()
	c.Require().NotNil(tr, "no tool_result in %v", ev)
	c.Eq("tu_1", tr.GetToolUseId(), "tool_use_id")
	c.True(tr.GetIsError(), "a failed tool's result must carry is_error")
	c.Eq("cat: /nope: No such file", tr.GetContent()[0].GetText().GetText(), "result text =")
}

// publishUserRow takes native blocks directly, with no SDK conversion in
// between: what the caller stored is what a durable consumer reads. A
// tool_result's text, its is_error flag and the image bytes all survive.
func TestPublishUserRowNative(t *testing.T) {
	c := assert.NewCollecting(t)
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), io.Discard, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	em.publishUserRow([]msg.Block{
		msg.Text{Text: "a prompt"},
		msg.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
		msg.ToolResult{
			ToolUseID: "tu_1",
			Content:   []msg.Block{msg.Text{Text: "tool output"}},
			IsError:   true,
		},
	}, store.Appended{CreatedAt: at})

	c.Require().Len(sink.events, 1, "native events")
	ev := sink.events[0]
	c.True(ev.GetTs().AsTime().Equal(at), "ts = %v, want the row's created_at %v", ev.GetTs().AsTime(), at)
	got := ev.GetUserMessage().GetContent()
	c.Require().Len(got, 3, "blocks")
	c.Eq("a prompt", got[0].GetText().GetText(), "text")
	c.EqDeep([]byte{1, 2, 3}, got[1].GetImage().GetData(), "image data")
	tr := got[2].GetToolResult()
	c.Require().NotNil(tr, "tool_result block")
	c.Eq("tu_1", tr.GetToolUseId(), "tool_use_id")
	c.True(tr.GetIsError(), "is_error")
	c.Require().Len(tr.GetContent(), 1, "tool_result content")
	c.Eq("tool output", tr.GetContent()[0].GetText().GetText(), "tool_result text")
	c.Eq(int32(2), got[2].GetIndex(), "block index")
}
