// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestModelHandlesUserMessage(t *testing.T) {
	m := &Model{renderer: newRenderer(), childID: "c_test"}
	m.handleEvent(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_UserMessage{
			UserMessage: &rafikiv1.UserMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hello"}},
				}},
			},
		},
	})
	if len(m.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(m.blocks))
	}
	if m.blocks[0].Kind != kindUser || m.blocks[0].Text != "hello" || !m.blocks[0].Final {
		t.Errorf("block = %+v, want kindUser text=hello final=true", m.blocks[0])
	}
}

func TestModelHandlesAssistantMessage(t *testing.T) {
	m := &Model{renderer: newRenderer(), childID: "c_test"}
	m.handleEvent(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi back"}},
				}},
			},
		},
	})
	if len(m.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(m.blocks))
	}
	if m.blocks[0].Kind != kindAssistant || !m.blocks[0].Final {
		t.Errorf("block kind=%v final=%v, want kindAssistant final=true", m.blocks[0].Kind, m.blocks[0].Final)
	}
}

func TestModelStreamingTurn(t *testing.T) {
	m := &Model{renderer: newRenderer(), childID: "c_test"}

	// TurnStart creates a non-final assistant block.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}},
	})
	if len(m.blocks) != 1 || m.blocks[0].Final {
		t.Fatal("TurnStart should create a non-final block")
	}

	// Content delta appends text.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_ContentBlockDelta{
			ContentBlockDelta: &rafikiv1.ContentBlockDelta{
				Delta: &rafikiv1.ContentBlockDelta_Text{Text: "streaming"},
			},
		},
	})
	if lastAssistant(m.blocks).Text != "streaming" {
		t.Errorf("text = %q, want streaming", lastAssistant(m.blocks).Text)
	}

	// TurnEnd finalizes.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}},
	})
	if !lastAssistant(m.blocks).Final {
		t.Fatal("TurnEnd should finalize the block")
	}
}

func TestModelToolExecution(t *testing.T) {
	m := &Model{renderer: newRenderer(), childID: "c_test"}

	// Setup streaming turn.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}},
	})

	// Tool starts.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_ToolExecutionStart{
			ToolExecutionStart: &rafikiv1.ToolExecutionStart{
				ToolUseId: "tu_1",
				Name:      "bash",
			},
		},
	})
	last := lastAssistant(m.blocks)
	if len(last.ToolCalls) != 1 || !last.ToolCalls[0].Running {
		t.Fatal("ToolStart should add a running tool call")
	}

	// Tool ends.
	m.handleEvent(&rafikiv1.Event{
		Payload: &rafikiv1.Event_ToolExecutionEnd{
			ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
				ToolUseId:  "tu_1",
				DurationMs: 1500,
				IsError:    false,
			},
		},
	})
	last = lastAssistant(m.blocks)
	if last.ToolCalls[0].Running || last.ToolCalls[0].DurationMs != 1500 {
		t.Errorf("ToolEnd should mark done, got running=%v durationMs=%d",
			last.ToolCalls[0].Running, last.ToolCalls[0].DurationMs)
	}
}

func TestRenderProducesOutput(t *testing.T) {
	r := newRenderer()
	blocks := []Block{
		{Kind: kindUser, Text: "hello", Final: true},
		{Kind: kindAssistant, Text: "hi back", Final: true},
	}
	out := r.renderBlocks(blocks, 2)
	if out == "" {
		t.Fatal("renderBlocks returned empty string")
	}
}

func TestFingerprintChanges(t *testing.T) {
	b1 := Block{Kind: kindAssistant, Text: "hello", Final: false}
	fp1 := b1.fingerprint()

	b2 := Block{Kind: kindAssistant, Text: "world", Final: false}
	fp2 := b2.fingerprint()
	if fp1 == fp2 {
		t.Error("different text should have different fingerprint")
	}

	b3 := Block{Kind: kindAssistant, Text: "hello", Final: true}
	if b1.fingerprint() == b3.fingerprint() {
		t.Error("final vs non-final should have different fingerprint")
	}
}
