// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"bytes"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type nativeCapture struct{ events []*rafikiv1.Event }

func (c *nativeCapture) Publish(ev *rafikiv1.Event) { c.events = append(c.events, ev) }

// The assistant's reply is the SUBSTANCE of a transcript, and it was never
// published. publishNative carried an AssistantMessage arm that nothing ever
// called, so the durable event log held child_spawned, user_message, the tool
// events and child_exited — and nothing the model actually said. A cockpit
// attaching to any agent saw its own prompts and no answers.
func TestEmitterPublishesNativeAssistantMessage(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.publishAssistant(&anthropic.Message{
		Role:       "assistant",
		StopReason: anthropic.StopReasonEndTurn,
		Content: []anthropic.ContentBlockUnion{
			{Type: "thinking", Thinking: "hmm"},
			{Type: "text", Text: "the answer"},
			{Type: "tool_use", ID: "tu_1", Name: "bash", Input: []byte(`{"cmd":"ls"}`)},
		},
	})

	am := findAssistant(t, sink.events)
	if got := am.GetRawStopReason(); got != "end_turn" {
		t.Errorf("raw stop reason = %q, want %q", got, "end_turn")
	}
	if am.GetStopReason() != rafikiv1.StopReason_STOP_REASON_END_TURN {
		t.Errorf("stop reason = %v, want END_TURN", am.GetStopReason())
	}
	if len(am.GetContent()) != 3 {
		t.Fatalf("got %d content blocks, want 3: %+v", len(am.GetContent()), am.GetContent())
	}
	if got := am.GetContent()[0].GetThinking().GetThinking(); got != "hmm" {
		t.Errorf("thinking = %q, want %q", got, "hmm")
	}
	if got := am.GetContent()[1].GetText().GetText(); got != "the answer" {
		t.Errorf("text = %q, want %q", got, "the answer")
	}
	tu := am.GetContent()[2].GetToolUse()
	if tu.GetId() != "tu_1" || tu.GetName() != "bash" || tu.GetInputJson() != `{"cmd":"ls"}` {
		t.Errorf("tool use = %+v", tu)
	}
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

	resp := &anthropic.Message{
		Role:       "assistant",
		StopReason: anthropic.StopReasonEndTurn,
		Content:    []anthropic.ContentBlockUnion{{Type: "text", Text: "streamed reply"}},
	}
	em.StreamStart(MapAssistantMessage(resp, "anthropic", nil))
	em.StreamEnd(MapAssistantMessage(resp, "anthropic", nil))
	em.publishAssistant(resp)

	am := findAssistant(t, sink.events)
	if got := am.GetContent()[0].GetText().GetText(); got != "streamed reply" {
		t.Errorf("text = %q, want %q", got, "streamed reply")
	}
}

// turn_end is durable and is how a non-TUI consumer sees a turn boundary at
// all. It carries the turn's usage so a cost consumer needs no second source.
func TestAgentEndPublishesTurnEnd(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	// Driven exactly as the engine drives it: the pi frames fold the usage,
	// then the durable publication, then the turn ends.
	resp := &anthropic.Message{
		Role:       "assistant",
		StopReason: anthropic.StopReasonEndTurn,
		Content:    []anthropic.ContentBlockUnion{{Type: "text", Text: "done"}},
		Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 3},
	}
	em.AssistantTurn(resp)
	em.publishAssistant(resp)
	em.AgentEnd()

	var te *rafikiv1.TurnEnd
	for _, ev := range sink.events {
		if t2 := ev.GetTurnEnd(); t2 != nil {
			te = t2
		}
	}
	if te == nil {
		t.Fatalf("no turn_end published; got %d events", len(sink.events))
	}
	if te.GetRawStopReason() != "end_turn" {
		t.Errorf("turn_end raw stop reason = %q, want %q", te.GetRawStopReason(), "end_turn")
	}
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

// Live tool output did not exist. ToolExecutionEnd carries a duration and an
// error flag and no text, and the user turn holding the result was persisted
// but never published — so watching an agent work showed every tool call with
// its arguments and its ✓/✗ and nothing of what it returned. Verified against
// a real daemon before the fix: every event arrived with resultLen=0.
func TestToolEndPublishesTheOutput(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), &out, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	em.ToolStart("tu_1", "bash", []byte(`{"command":"false"}`))
	em.ToolEnd("tu_1", "bash", "cat: /nope: No such file", true)

	var tr *rafikiv1.ToolResultBlock
	for _, ev := range sink.events {
		for _, cb := range ev.GetUserMessage().GetContent() {
			if b := cb.GetToolResult(); b != nil {
				tr = b
			}
		}
	}
	if tr == nil {
		t.Fatalf("no tool_result published; got %d events", len(sink.events))
	}
	if tr.GetToolUseId() != "tu_1" {
		t.Errorf("tool_use_id = %q, want tu_1", tr.GetToolUseId())
	}
	if !tr.GetIsError() {
		t.Error("a failed tool's result must carry is_error")
	}
	if got := tr.GetContent()[0].GetText().GetText(); got != "cat: /nope: No such file" {
		t.Errorf("result text = %q", got)
	}
}
