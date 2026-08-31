// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/session"
)

func textEvent(childID, text string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}},
	}
}

func TestApplyUserMessage(t *testing.T) {
	s := session.New("c_test")
	s.Apply(textEvent("c_test", "hello"))
	if len(s.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(s.Blocks))
	}
	b := s.Blocks[0]
	if b.Kind != session.KindUser || b.Text != "hello" || !b.Final {
		t.Errorf("block = %+v, want KindUser text=hello final=true", b)
	}
}

func TestApplyIgnoresAnotherChildsEvent(t *testing.T) {
	s := session.New("c_mine")
	s.Apply(textEvent("c_theirs", "not for me"))
	if len(s.Blocks) != 0 {
		t.Fatalf("blocks = %d, want 0 -- a session must ignore another child's event", len(s.Blocks))
	}
}

func TestApplyAssistantMessage(t *testing.T) {
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi back"}},
			}},
		}},
	})
	if len(s.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(s.Blocks))
	}
	if s.Blocks[0].Kind != session.KindAssistant || !s.Blocks[0].Final {
		t.Errorf("kind=%v final=%v, want KindAssistant final=true", s.Blocks[0].Kind, s.Blocks[0].Final)
	}
}

func TestStreamingTurn(t *testing.T) {
	s := session.New("c_test")

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}})
	if len(s.Blocks) != 1 || s.Blocks[0].Final {
		t.Fatal("TurnStart should create a non-final block")
	}

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: "streaming"},
		}}})
	if got := s.LastAssistant().Text; got != "streaming" {
		t.Errorf("text = %q, want streaming", got)
	}

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}})
	if !s.LastAssistant().Final {
		t.Fatal("TurnEnd should finalize the block")
	}
}

func TestToolExecution(t *testing.T) {
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}})

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{
			ToolUseId: "tu_1", Name: "bash",
		}}})
	last := s.LastAssistant()
	if len(last.ToolCalls) != 1 || !last.ToolCalls[0].Running {
		t.Fatal("ToolExecutionStart should add a running tool call")
	}

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
			ToolUseId: "tu_1", DurationMs: 1500,
		}}})
	last = s.LastAssistant()
	if last.ToolCalls[0].Running || last.ToolCalls[0].DurationMs != 1500 {
		t.Errorf("running=%v durationMs=%d, want false/1500",
			last.ToolCalls[0].Running, last.ToolCalls[0].DurationMs)
	}
}

func TestCursorTracksHighestOrdinalAndZeroIsLegal(t *testing.T) {
	s := session.New("c_test")
	if s.HasCursor {
		t.Fatal("a fresh session must have no cursor")
	}

	zero := int32(0)
	ev := textEvent("c_test", "first")
	ev.Ordinal = &zero
	s.Apply(ev)
	if !s.HasCursor || s.Cursor != 0 {
		t.Fatalf("cursor = %d hasCursor = %v, want 0/true -- ordinal 0 is legal",
			s.Cursor, s.HasCursor)
	}

	five := int32(5)
	ev5 := textEvent("c_test", "later")
	ev5.Ordinal = &five
	s.Apply(ev5)

	two := int32(2)
	ev2 := textEvent("c_test", "out of order")
	ev2.Ordinal = &two
	s.Apply(ev2)

	if s.Cursor != 5 {
		t.Errorf("cursor = %d, want 5 -- the cursor must never go backwards", s.Cursor)
	}
}

// The rail and focus subscriptions overlap on the durable tier, so a focused
// child's turn_end and error events arrive on BOTH. Without ordinal dedupe an
// error appends its block twice and the transcript grows phantom entries.
func TestDuplicateOrdinalIsIgnored(t *testing.T) {
	s := session.New("c_1")
	errEv := func(ord int32) *rafikiv1.Event {
		return &rafikiv1.Event{ChildId: "c_1", Ordinal: &ord,
			Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
				Code: "boom", Message: "upstream died"}}}
	}
	s.Apply(errEv(4))
	s.Apply(errEv(4)) // same ordinal, delivered by the other subscription
	if len(s.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 -- the duplicate must be dropped", len(s.Blocks))
	}
	s.Apply(errEv(5))
	if len(s.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 -- a genuinely new ordinal must still apply", len(s.Blocks))
	}
}

// Anthropic puts tool_result in the USER message following the tool_use, and
// TextFromContent reads text blocks only — so those messages rendered as EMPTY
// user bubbles and every tool's output was dropped. One blank bubble per tool
// call, no output anywhere.
func TestToolResultAttachesToItsCallAndAddsNoBubble(t *testing.T) {
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash", InputJson: `{"command":"ls"}`}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
				ToolUseId: "tu_1",
				Content: []*rafikiv1.ContentBlock{{
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "a.go\nb.go"}}}},
			}}},
		}},
	}})

	if len(s.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1 — a results-only message must add no user bubble", len(s.Blocks))
	}
	calls := s.Blocks[0].ToolCalls
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].Input != `{"command":"ls"}` {
		t.Errorf("input = %q, want the tool's arguments", calls[0].Input)
	}
	if calls[0].Result != "a.go\nb.go" {
		t.Errorf("result = %q, want the tool's output", calls[0].Result)
	}
}

// A user message with real text alongside results still renders its text.
func TestUserTextAlongsideAResultStillRenders(t *testing.T) {
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{
			{Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{ToolUseId: "tu_x"}}},
			{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "and also this"}}},
		}},
	}})
	if len(s.Blocks) != 1 || s.Blocks[0].Text != "and also this" {
		t.Fatalf("blocks = %+v, want the user's text preserved", s.Blocks)
	}
}

// The assistant message naming a tool_use is published BEFORE the tool runs, so
// tool_execution_start is normally not new. Appending unconditionally listed
// every call twice — invisible while fundi published no assistant messages,
// immediate once it did.
func TestToolExecutionStartDoesNotDuplicateAKnownCall(t *testing.T) {
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash"}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_ToolExecutionStart{
		ToolExecutionStart: &rafikiv1.ToolExecutionStart{ToolUseId: "tu_1", Name: "bash"}}})

	if n := len(s.Blocks[0].ToolCalls); n != 1 {
		t.Fatalf("got %d tool calls for one tool_use, want 1", n)
	}
	if !s.Blocks[0].ToolCalls[0].Running {
		t.Error("tool_execution_start must mark the known call running")
	}
}

// tool_execution_end is the more direct witness of a failure — it carries the
// tool's own error. A stored tool_result block whose is_error is absent must
// not turn that ✗ back into a ✓: it is the one direction that must never
// happen silently.
func TestAToolResultCannotDowngradeAKnownFailure(t *testing.T) {
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash"}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_ToolExecutionEnd{
		ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{ToolUseId: "tu_1", IsError: true}}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
				ToolUseId: "tu_1", // is_error absent
				Content: []*rafikiv1.ContentBlock{{
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "boom"}}}},
			}}},
		}},
	}})

	if !s.Blocks[0].ToolCalls[0].IsError {
		t.Error("a tool_result with no is_error downgraded a known failure to success")
	}
	if s.Blocks[0].ToolCalls[0].Result != "boom" {
		t.Errorf("result = %q, want the tool's output", s.Blocks[0].ToolCalls[0].Result)
	}
}
