package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{rows: []ConversationSummaryRow{
		{ID: "conv-1", Model: "m1", Turns: 3, TotalCostUSD: 0.0123, FirstMessage: "fix the bug"},
	}}
	tool, err := (ConversationSearchBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	res, err := tool.Execute(context.Background(), ToolInput(`{"limit":10,"model":"m1"}`))
	c.Require().NoError(err, "Execute")
	c.False(!strings.Contains(res.Text, "1 matched") || !strings.Contains(res.Text, "fix the bug"), "Execute text = %q, want the count and the first message", res.Text)
	if fake.query.Limit != 10 || fake.query.Model != "m1" {
		t.Errorf("query forwarded = %+v, want limit 10 and model m1", fake.query)
	}
}

// TestConversationSearchBindsMinTokens pins that the schema's advertised
// min_tokens argument actually reaches the filter. The field carried no json
// tag, and encoding/json does not cross underscores, so "min_tokens" never
// bound to MinTokens and an agent's token filter was silently dropped -- this
// test fails (MinTokens 0) against that pre-fix behaviour.
func TestConversationSearchBindsMinTokens(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})

	_, err := tool.Execute(context.Background(), ToolInput(`{"min_tokens":2500}`))
	c.Require().NoError(err, "Execute")
	if fake.query.MinTokens != 2500 {
		t.Errorf("MinTokens = %d, want 2500 (the tool must bind the min_tokens argument)", fake.query.MinTokens)
	}
}

// TestConversationSearchBindsSinceUntil pins that the tool's since/until
// arguments actually bind to the filter and reach the reader. Before the fix
// these fields carried no json tag, so encoding/json (which matches by
// case-insensitive FIELD NAME and does not cross underscores) never bound a
// `since`/`until` key to them and they were silently ignored -- so this test
// fails (nil filter times) against that pre-fix behaviour.
func TestConversationSearchBindsSinceUntil(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})

	_, err := tool.Execute(context.Background(), ToolInput(
		`{"since":"2026-01-02T03:04:05Z","until":"2026-01-03T06:07:08Z"}`))
	c.Require().NoError(err, "Execute")
	since, _ := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	until, _ := time.Parse(time.RFC3339, "2026-01-03T06:07:08Z")
	if fake.query.Since == nil || !fake.query.Since.Equal(since) {
		t.Errorf("Since = %v, want %v (the tool must bind the since argument)", fake.query.Since, since)
	}
	if fake.query.Until == nil || !fake.query.Until.Equal(until) {
		t.Errorf("Until = %v, want %v (the tool must bind the until argument)", fake.query.Until, until)
	}
}

// TestConversationSearchBlankSinceUntilIsUnbounded pins that an absent key OR a
// blanked optional argument ("") means unbounded, never an error.
func TestConversationSearchBlankSinceUntilIsUnbounded(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"absent", ToolInput(`{}`)},
		{"empty strings", ToolInput(`{"since":"","until":""}`)},
		{"null", ToolInput(`{"since":null,"until":null}`)},
	} {
		fake := &fakeConversationReader{}
		tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().NoError(err, "%s: Execute", tc.name)
		c.True(fake.query.Since == nil && fake.query.Until == nil,
			"%s: since/until = %v/%v, want both nil (unbounded)", tc.name, fake.query.Since, fake.query.Until)
	}
}

// TestConversationSearchMalformedSinceIsAnError pins that a genuinely malformed
// non-empty bound fails the whole call loudly rather than being ignored.
func TestConversationSearchMalformedSinceIsAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"date only", ToolInput(`{"since":"2026-01-01"}`)},
		{"garbage", ToolInput(`{"until":"not-a-time"}`)},
		{"wrong type", ToolInput(`{"since":123}`)},
	} {
		fake := &fakeConversationReader{}
		tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().Error(err, "%s: Execute returned no error", tc.name)
		c.StrContains(err.Error(), "conversation_search", "%s: error does not name the tool", tc.name)
	}
}

func TestConversationSearchEmptyResult(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput("{}"))
	c.Require().NoError(err, "Execute")
	c.Eq("no conversations matched", res.Text, "Execute text")
}

func TestConversationSearchReaderErrorIsAnError(t *testing.T) {
	// A routed tool must return an ERROR, never the error's text as a
	// successful result -- agentloop computes is_error from err != nil.
	fake := &fakeConversationReader{err: context.DeadlineExceeded}
	tool, _ := ConversationSearchBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	_, err := tool.Execute(context.Background(), nil)
	assert.NewAborting(t).Error(err, "Execute with a failing reader returned no error")
}

func TestConversationExportRequiresConversationID(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := ConversationExportBlueprint{}.Materialize(ToolOpts{Conversations: &fakeConversationReader{}})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without conversation_id returned no error")
	}
	def := tool.InputSchema()
	c.False(len(def.Required) != 1 || def.Required[0] != "conversation_id", "schema.Required = %v, want [conversation_id]", def.Required)
}

func TestConversationExportMarshalsTheTranscript(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{tr: &ConversationTranscript{
		ConversationID: "conv-9",
		Turns: []ConversationTranscriptTurn{
			{Ordinal: 0, Role: "user", Content: []byte(`"hello"`)},
		},
		AvailableSkills: []string{"deploy"},
	}}
	tool, _ := ConversationExportBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput(`{"conversation_id":"conv-9"}`))
	c.Require().NoError(err, "Execute")
	c.False(!strings.Contains(res.Text, `"ConversationID": "conv-9"`) || !strings.Contains(res.Text, "deploy"), "Execute text = %q, want the marshalled transcript", res.Text)
}

func TestConversationQueryBlueprintDeclinesWithoutAReader(t *testing.T) {
	if tool, err := (ConversationQueryBlueprint{}).Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("query Materialize with no Conversations = (%v, %v), want (nil, nil)", tool, err)
	}
}

func TestConversationQueryRequiresName(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: &fakeConversationReader{}})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without name returned no error")
	}
	def := tool.InputSchema()
	c.False(len(def.Required) != 1 || def.Required[0] != "name", "schema.Required = %v, want [name]", def.Required)
}

func TestConversationQueryMaterializesAndRendersTheCatalogue(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{runResult: CatalogueResult{
		Columns: []CatalogueColumn{{Name: "tool"}, {Name: "calls"}, {Name: "avg"}},
		Rows: [][]CatalogueEntry{
			{{Str: "bash"}, {Int: 3, IsInt: true}, {Float: 0.75, IsFloat: true}},
		},
	}}
	tool, err := (ConversationQueryBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	res, err := tool.Execute(context.Background(), ToolInput(
		`{"name":"tools","since":"2026-01-02T03:04:05Z","until":"2026-01-03T03:04:05Z","model":"m1","source":"agent","path":"proxy"}`))
	c.Require().NoError(err, "Execute")
	c.False(!strings.Contains(res.Text, "tool\tcalls\tavg\n") || !strings.Contains(res.Text, "bash\t3\t0.75\n"), "Execute text = %q, want the header row and the typed cells", res.Text)
	c.Eq("tools", fake.runName, "query name forwarded")
	since, _ := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	until, _ := time.Parse(time.RFC3339, "2026-01-03T03:04:05Z")
	if f := fake.runFilter; f.Since == nil || !f.Since.Equal(since) || f.Until == nil || !f.Until.Equal(until) || f.Model != "m1" || f.Source != "agent" || f.Path != "proxy" {
		t.Errorf("filter forwarded = %+v, want every field the input carried (until in particular)", f)
	}
}

// TestConversationQueryBlankSinceUntilIsUnbounded pins that an absent key OR a
// blanked optional argument ("") means unbounded, never an error. Before the
// rfc3339Time fix a "" reached time.Time's JSON unmarshaller and hard-failed
// the call with `parsing time "" ... cannot parse`, so this test fails against
// the pre-fix source.
func TestConversationQueryBlankSinceUntilIsUnbounded(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"absent", ToolInput(`{"name":"tools"}`)},
		{"empty strings", ToolInput(`{"name":"tools","since":"","until":""}`)},
		{"null", ToolInput(`{"name":"tools","since":null,"until":null}`)},
	} {
		fake := &fakeConversationReader{}
		tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().NoError(err, "%s: Execute", tc.name)
		c.True(fake.runFilter.Since == nil && fake.runFilter.Until == nil,
			"%s: since/until = %v/%v, want both nil (unbounded)", tc.name, fake.runFilter.Since, fake.runFilter.Until)
	}
}

// TestConversationQueryMalformedSinceIsAnError pins that a genuinely malformed
// non-empty bound fails the whole call loudly rather than being ignored.
func TestConversationQueryMalformedSinceIsAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"date only", ToolInput(`{"name":"tools","since":"2026-01-01"}`)},
		{"garbage", ToolInput(`{"name":"tools","until":"not-a-time"}`)},
		{"wrong type", ToolInput(`{"name":"tools","since":123}`)},
	} {
		fake := &fakeConversationReader{}
		tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().Error(err, "%s: Execute returned no error", tc.name)
		c.StrContains(err.Error(), "conversation_query", "%s: error does not name the tool", tc.name)
	}
}

func TestConversationQueryEmptyResult(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationReader{}
	tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"coverage"}`))
	c.Require().NoError(err, "Execute")
	c.Eq("no rows", res.Text, "Execute text")
}

func TestConversationQueryReaderErrorIsAnError(t *testing.T) {
	// A routed tool must return an ERROR, never the error's text as a
	// successful result -- agentloop computes is_error from err != nil.
	fake := &fakeConversationReader{err: context.DeadlineExceeded}
	tool, _ := ConversationQueryBlueprint{}.Materialize(ToolOpts{Conversations: fake})
	_, err := tool.Execute(context.Background(), ToolInput(`{"name":"tools"}`))
	assert.NewAborting(t).Error(err, "Execute with a failing reader returned no error")
}
