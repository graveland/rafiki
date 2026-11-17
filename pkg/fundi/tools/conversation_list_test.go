// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakeConversationListReader is the reader the list tests bind. The search/
// export/query fake (fakeConversationReader) lives in
// conversation_query_test.go with its fields declared there; the list tests
// need their own captured query and page, so they carry this one.
type fakeConversationListReader struct {
	query ConversationListQuery
	page  ConversationListPage
	err   error
}

func (f *fakeConversationListReader) ConversationSearch(context.Context, ConversationQuery) ([]ConversationSummaryRow, error) {
	return nil, nil
}

func (f *fakeConversationListReader) ConversationList(_ context.Context, q ConversationListQuery) (ConversationListPage, error) {
	f.query = q
	return f.page, f.err
}

func (f *fakeConversationListReader) ConversationExport(context.Context, string) (*ConversationTranscript, error) {
	return nil, nil
}

func (f *fakeConversationListReader) RunQuery(context.Context, string, CatalogueFilter) (CatalogueResult, error) {
	return CatalogueResult{}, nil
}

// ConversationList is the shared fake's list method, declared here beside the
// list tests: the fake's own file pins its fields, and a method may live in
// any file of the package.
func (f *fakeConversationReader) ConversationList(_ context.Context, q ConversationListQuery) (ConversationListPage, error) {
	return ConversationListPage{}, nil
}

func TestConversationListDeclinesWithoutAReader(t *testing.T) {
	if tool, err := (ConversationListBlueprint{}).Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("list Materialize with no Conversations = (%v, %v), want (nil, nil)", tool, err)
	}
}

// TestConversationListBindsAndMarshalsThePage covers the whole argument
// surface in one call: every schema argument must reach the reader, and the
// page must come back as its JSON with the snake_case field names the
// description speaks of (child_count, next_cursor).
func TestConversationListBindsAndMarshalsThePage(t *testing.T) {
	c := assert.NewCollecting(t)
	since, err := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	c.Require().NoError(err, "parse since")
	until, err := time.Parse(time.RFC3339, "2026-01-03T06:07:08Z")
	c.Require().NoError(err, "parse until")
	fake := &fakeConversationListReader{page: ConversationListPage{
		Rows: []ConversationListRow{{
			ID: "conv-1", Name: "fix the bug", Model: "m1", Owner: "alice",
			LastActivity: until, Turns: 3, CostUSD: 0.0123,
			ChildCount: 2, Kind: "fundi", State: "no_agent",
		}},
		NextCursor: "cur-2",
	}}
	tool, err := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	res, err := tool.Execute(context.Background(), ToolInput(`{
		"limit": 10,
		"cursor": "cur-1",
		"sort": "created",
		"parent_conversation_id": "conv-0",
		"query": "deploy",
		"states": ["open", "closed"],
		"kinds": ["fundi", "claude"],
		"name_contains": "fix",
		"cwd_contains": "/home",
		"labels": {"rafiki/executor": "e-1"},
		"since": "2026-01-02T03:04:05Z",
		"until": "2026-01-03T06:07:08Z"
	}`))
	c.Require().NoError(err, "Execute")
	c.EqDiff(ConversationListQuery{
		Limit: 10, Cursor: "cur-1", Sort: "created",
		ParentConversationID: "conv-0", Query: "deploy",
		States:       []string{"open", "closed"},
		Kinds:        []string{"fundi", "claude"},
		NameContains: "fix", CwdContains: "/home",
		Labels: map[string]string{"rafiki/executor": "e-1"},
		Since:  &since, Until: &until,
	}, fake.query, "query forwarded")
	c.False(!strings.Contains(res.Text, `"child_count": 2`) ||
		!strings.Contains(res.Text, `"next_cursor": "cur-2"`) ||
		!strings.Contains(res.Text, `"state": "no_agent"`) ||
		!strings.Contains(res.Text, `"id": "conv-1"`),
		"Execute text = %q, want the row fields and the cursor", res.Text)
}

// TestConversationListBlankSinceUntilIsUnbounded pins that an absent key OR a
// blanked optional argument ("") means unbounded, never an error.
func TestConversationListBlankSinceUntilIsUnbounded(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"absent", ToolInput(`{}`)},
		{"empty strings", ToolInput(`{"since":"","until":""}`)},
		{"null", ToolInput(`{"since":null,"until":null}`)},
	} {
		fake := &fakeConversationListReader{}
		tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().NoError(err, "%s: Execute", tc.name)
		c.True(fake.query.Since == nil && fake.query.Until == nil,
			"%s: since/until = %v/%v, want both nil (unbounded)", tc.name, fake.query.Since, fake.query.Until)
	}
}

// TestConversationListRefusesABadSort pins that a sort outside the vocabulary
// is refused with an error naming the field.
func TestConversationListRefusesABadSort(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationListReader{}
	tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	_, err := tool.Execute(context.Background(), ToolInput(`{"sort":"newest"}`))
	c.Require().Error(err, "Execute with a bad sort returned no error")
	c.StrContains(err.Error(), "sort", "the error does not name the field")
	c.StrContains(err.Error(), "conversation_list", "the error does not name the tool")
	c.Zero(len(fake.query.States), "the reader must not have been called")
}

// TestConversationListRefusesABadState pins that a state outside the
// vocabulary is refused with an error naming the field, and that the valid
// values alongside a bad one do not slip through.
func TestConversationListRefusesABadState(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"single bad value", ToolInput(`{"states":["archived"]}`)},
		{"bad value beside a good one", ToolInput(`{"states":["open","archived"]}`)},
	} {
		fake := &fakeConversationListReader{}
		tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().Error(err, "%s: Execute returned no error", tc.name)
		c.StrContains(err.Error(), "states", "%s: the error does not name the field", tc.name)
		c.StrContains(err.Error(), "conversation_list", "%s: the error does not name the tool", tc.name)
	}
}

// TestConversationListRefusesABadBound pins that a malformed non-empty
// since/until fails the whole call loudly with an error naming the field --
// the shared rfc3339Time cannot do that, which is why the list tool parses
// its bounds itself.
func TestConversationListRefusesABadBound(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name  string
		in    ToolInput
		field string
	}{
		{"since, date only", ToolInput(`{"since":"2026-01-01"}`), "since"},
		{"until, garbage", ToolInput(`{"until":"not-a-time"}`), "until"},
		{"since, wrong type", ToolInput(`{"since":123}`), "since"},
	} {
		fake := &fakeConversationListReader{}
		tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().Error(err, "%s: Execute returned no error", tc.name)
		c.StrContains(err.Error(), tc.field, "%s: the error does not name the field", tc.name)
		c.StrContains(err.Error(), "conversation_list", "%s: the error does not name the tool", tc.name)
	}
}

// TestConversationListRefusesANegativeLimit pins that a negative page size is
// refused with an error naming the field, exactly as the Connect face refuses
// it with InvalidArgument, while 0 still asks for the default. Without the
// check, insights treats every limit <= 0 as the default and the two faces
// silently disagree on the same input.
func TestConversationListRefusesANegativeLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   ToolInput
	}{
		{"minus one", ToolInput(`{"limit":-1}`)},
		{"large negative", ToolInput(`{"limit":-500}`)},
	} {
		fake := &fakeConversationListReader{}
		tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
		_, err := tool.Execute(context.Background(), tc.in)
		c.Require().Error(err, "%s: Execute returned no error", tc.name)
		c.StrContains(err.Error(), "limit", "%s: the error does not name the field", tc.name)
		c.StrContains(err.Error(), "conversation_list", "%s: the error does not name the tool", tc.name)
		c.Eq(0, fake.query.Limit, "%s: the reader must not have been called", tc.name)
	}

	// 0 is the default page size, not a mistake: it reaches the reader as 0.
	fake := &fakeConversationListReader{}
	tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	_, err := tool.Execute(context.Background(), ToolInput(`{"limit":0}`))
	c.Require().NoError(err, "Execute with limit 0")
	c.Eq(0, fake.query.Limit, "limit 0 must reach the reader as 0 (the default page size)")
}

// TestConversationListEmptyPageStillCarriesRows pins the output contract: an
// empty page marshals as an empty rows array with an empty next_cursor, not a
// bare null, so the model's paging pattern never has a special case.
func TestConversationListEmptyPageStillCarriesRows(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeConversationListReader{}
	tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	res, err := tool.Execute(context.Background(), ToolInput("{}"))
	c.Require().NoError(err, "Execute")
	c.StrContains(res.Text, `"rows": []`, "empty page")
	c.StrContains(res.Text, `"next_cursor": ""`, "empty page")
}

// TestConversationListSchemaAdvertisesTheVocabularies pins the transport
// vocabulary the schema advertises: a model reading the schema alone must see
// the same sort values Execute accepts, and the states array must declare its
// element vocabulary in the description (SchemaProperty has no enum for array
// items).
func TestConversationListSchemaAdvertisesTheVocabularies(t *testing.T) {
	c := assert.NewCollecting(t)
	var sortSeen bool
	for _, p := range (ConversationListBlueprint{}).InputSchema().Properties {
		switch p.Name {
		case "sort":
			sortSeen = true
			c.EqDiff(listSorts, p.Enum, "sort enum")
		case "states":
			c.StrContains(p.Description, strings.Join(listStates, "\", \""), "states description")
		}
	}
	c.True(sortSeen, "conversation_list has no sort property")
}

func TestConversationListReaderErrorIsAnError(t *testing.T) {
	// A routed tool must return an ERROR, never the error's text as a
	// successful result -- agentloop computes is_error from err != nil.
	fake := &fakeConversationListReader{err: errors.New("boom")}
	tool, _ := (ConversationListBlueprint{}).Materialize(ToolOpts{Conversations: fake})
	_, err := tool.Execute(context.Background(), ToolInput("{}"))
	assert.NewAborting(t).Error(err, "Execute with a failing reader returned no error")
}

// TestConversationListDescriptionNeedsNoScopeExcision pins the composition
// contract the MCP face relies on: the blueprint text carries NO ownership
// claim, so the face's override is the scope note plus the verbatim text with
// nothing excised. The phrase below is mcpSearchScopeStart's exact text
// (cmd/rafikid): if the blueprint ever grows that claim, the note and the
// claim would sit on one description and the face's composition would ship
// the falsehood -- this fails loudly instead.
func TestConversationListDescriptionNeedsNoScopeExcision(t *testing.T) {
	c := assert.NewCollecting(t)
	desc := conversationListDescription
	c.NotStrContains(desc, "Results are scoped to conversations you own",
		"conversation_list ships a fundi-only ownership claim; the MCP face's override needs an excision again")
	// The nesting and paging contract is the tool's whole read path; if the
	// mandated wording drifts, these are the phrases the face's text must
	// still carry.
	c.StrContains(desc, "each with child_count", "the row-nesting contract is gone")
	c.StrContains(desc, "Pass next_cursor back as cursor", "the paging contract is gone")
	c.StrContains(desc, "Use conversation_export or recall to read content", "the content pointer is gone")
}
