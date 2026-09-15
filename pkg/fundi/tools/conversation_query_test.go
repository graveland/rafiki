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

	// RunQuery capture. Named separately from the search/export fields so a
	// test can tell which method a tool actually called.
	runName   string
	runFilter CatalogueFilter
	runResult CatalogueResult
}

func (f *fakeConversationReader) ConversationSearch(_ context.Context, q ConversationQuery) ([]ConversationSummaryRow, error) {
	f.query = q
	return f.rows, f.err
}

func (f *fakeConversationReader) ConversationExport(_ context.Context, _ string) (*ConversationTranscript, error) {
	return f.tr, f.err
}

func (f *fakeConversationReader) RunQuery(_ context.Context, name string, f2 CatalogueFilter) (CatalogueResult, error) {
	f.runName, f.runFilter = name, f2
	return f.runResult, f.err
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

func TestConversationQueryBlueprintDeclinesWithoutAReader(t *testing.T) {
	if tool, err := (ConversationQueryBlueprint{}).Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("query Materialize with no Conversations = (%v, %v), want (nil, nil)", tool, err)
	}
}

func TestConversationQueryRequiresName(t *testing.T) {
	tool, err := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: &fakeConversationReader{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without name returned no error")
	}
	def := tool.InputSchema()
	if len(def.Required) != 1 || def.Required[0] != "name" {
		t.Errorf("schema.Required = %v, want [name]", def.Required)
	}
}

func TestConversationQueryMaterializesAndRendersTheCatalogue(t *testing.T) {
	fake := &fakeConversationReader{runResult: CatalogueResult{
		Columns: []CatalogueColumn{{Name: "tool"}, {Name: "calls"}, {Name: "avg"}},
		Rows: [][]CatalogueEntry{
			{{Str: "bash"}, {Int: 3, IsInt: true}, {Float: 0.75, IsFloat: true}},
		},
	}}
	tool, err := (ConversationQueryBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(
		`{"name":"tools","since_unix":100,"until_unix":200,"model":"m1","source":"agent","path":"proxy"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "tool\tcalls\tavg\n") || !strings.Contains(res.Text, "bash\t3\t0.75\n") {
		t.Errorf("Execute text = %q, want the header row and the typed cells", res.Text)
	}
	if fake.runName != "tools" {
		t.Errorf("query name forwarded = %q, want tools", fake.runName)
	}
	if f := fake.runFilter; f.SinceUnix != 100 || f.UntilUnix != 200 || f.Model != "m1" || f.Source != "agent" || f.Path != "proxy" {
		t.Errorf("filter forwarded = %+v, want every field the input carried (until_unix in particular)", f)
	}
}

func TestConversationQueryEmptyResult(t *testing.T) {
	fake := &fakeConversationReader{}
	tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"coverage"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Text != "no rows" {
		t.Errorf("Execute text = %q, want the no-rows answer", res.Text)
	}
}

func TestConversationQueryReaderErrorIsAnError(t *testing.T) {
	// A routed tool must return an ERROR, never the error's text as a
	// successful result -- agentloop computes is_error from err != nil.
	fake := &fakeConversationReader{err: context.DeadlineExceeded}
	tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	if _, err := tool.Execute(context.Background(), ToolInput(`{"name":"tools"}`)); err == nil {
		t.Fatal("Execute with a failing reader returned no error")
	}
}
