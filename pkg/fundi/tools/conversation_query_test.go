package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeConversationReader struct {
	rows  []ConversationSummaryRow
	tr    *ConversationTranscript
	err   error
	query ConversationQuery
}

func (f *fakeConversationReader) ConversationSearch(_ context.Context, q ConversationQuery) ([]ConversationSummaryRow, error) {
	f.query = q
	return f.rows, f.err
}

func (f *fakeConversationReader) ConversationExport(_ context.Context, _ string) (*ConversationTranscript, error) {
	return f.tr, f.err
}

func TestConversationToolsDeclineWithoutAReader(t *testing.T) {
	if tool, err := (ConversationSearchBlueprint{}).Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("search Materialize with no Conversations = (%v, %v), want (nil, nil)", tool, err)
	}
	if tool, err := (ConversationExportBlueprint{}).Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("export Materialize with no Conversations = (%v, %v), want (nil, nil)", tool, err)
	}
}

func TestConversationSearchMaterializesAndFormatsRows(t *testing.T) {
	fake := &fakeConversationReader{rows: []ConversationSummaryRow{
		{ID: "conv-1", Model: "m1", Turns: 3, TotalCostUSD: 0.0123, FirstMessage: "fix the bug"},
	}}
	tool, err := (ConversationSearchBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"limit":10,"model":"m1"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "1 matched") || !strings.Contains(res.Text, "fix the bug") {
		t.Errorf("Execute text = %q, want the count and the first message", res.Text)
	}
	if fake.query.Limit != 10 || fake.query.Model != "m1" {
		t.Errorf("query forwarded = %+v, want limit 10 and model m1", fake.query)
	}
}

func TestConversationSearchEmptyResult(t *testing.T) {
	fake := &fakeConversationReader{}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput("{}"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Text != "no conversations matched" {
		t.Errorf("Execute text = %q, want the no-match answer", res.Text)
	}
}

func TestConversationSearchReaderErrorIsAnError(t *testing.T) {
	// A routed tool must return an ERROR, never the error's text as a
	// successful result -- agentloop computes is_error from err != nil.
	fake := &fakeConversationReader{err: context.DeadlineExceeded}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("Execute with a failing reader returned no error")
	}
}

func TestConversationExportRequiresConversationID(t *testing.T) {
	tool, err := ConversationExportBlueprint{}.Materialize(ToolOpts{Conversations: &fakeConversationReader{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without conversation_id returned no error")
	}
	def := tool.InputSchema()
	if len(def.Required) != 1 || def.Required[0] != "conversation_id" {
		t.Errorf("schema.Required = %v, want [conversation_id]", def.Required)
	}
}

func TestConversationExportMarshalsTheTranscript(t *testing.T) {
	fake := &fakeConversationReader{tr: &ConversationTranscript{
		ConversationID: "conv-9",
		Turns: []ConversationTranscriptTurn{
			{Ordinal: 0, Role: "user", Content: []byte(`"hello"`)},
		},
		AvailableSkills: []string{"deploy"},
	}}
	tool, _ := ConversationExportBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput(`{"conversation_id":"conv-9"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, `"ConversationID": "conv-9"`) || !strings.Contains(res.Text, "deploy") {
		t.Errorf("Execute text = %q, want the marshalled transcript", res.Text)
	}
}
