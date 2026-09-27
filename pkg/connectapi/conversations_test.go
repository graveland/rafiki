// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeConversationInsights struct {
	gotFilter connectapi.ConversationSearchFilter
	gotID     string
	gotName   string
	gotQuery  connectapi.CatalogueFilter

	rows []connectapi.ConversationSummaryRow
	tr   connectapi.TranscriptRow
	ok   bool
	err  error

	result connectapi.CatalogueResult
}

func (f *fakeConversationInsights) Search(_ context.Context, flt connectapi.ConversationSearchFilter) ([]connectapi.ConversationSummaryRow, error) {
	f.gotFilter = flt
	return f.rows, f.err
}

func (f *fakeConversationInsights) Export(_ context.Context, conversationID string) (connectapi.TranscriptRow, bool, error) {
	f.gotID = conversationID
	return f.tr, f.ok, f.err
}

func (f *fakeConversationInsights) RunQuery(_ context.Context, name string, flt connectapi.CatalogueFilter) (connectapi.CatalogueResult, error) {
	f.gotName = name
	f.gotQuery = flt
	return f.result, f.err
}

func newConversationsServer(f *fakeConversationInsights) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetConversationInsights(f)
	return s
}

// ptrInt64 builds the *int64 a proto `optional int64` field decodes to.
func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

func TestConversationSearchNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationSearch unwired err = %v, want", err)
}

func TestConversationSearchMapsRowsAndFilter(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{rows: []connectapi.ConversationSummaryRow{{
		ID: "c1", Name: "fix the lease", Owner: "brent", Persona: "worker",
		Source: "proxy", Model: "openrouter/x/glm", Status: "idle", DrivenBy: "fundi",
		CreatedAtUnix: 1757000000, Turns: 7,
		InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 800,
		CacheHitRatio: 0.8, TotalCostUSD: 0.0123, FirstMessage: "please fix",
	}}}
	s := newConversationsServer(f)

	resp, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{Owner: "brent", Limit: 20}))
	c.Require().NoError(err, "ConversationSearch")
	c.Eq("brent", f.gotFilter.Owner, "filter Owner")
	c.Eq(20, f.gotFilter.Limit, "filter Limit")
	got := resp.Msg.GetRows()
	c.Require().Len(got, 1, "rows = %d, want 1", len(got))
	c.Eq("c1", got[0].GetId(), "Id")
	c.Eq("brent", got[0].GetOwner(), "Owner")
	c.Eq(0.0123, got[0].GetTotalCostUsd(), "TotalCostUsd")
}

// The server clamps a caller's limit; it never forwards an unbounded one.
func TestConversationSearchClampsLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{}
	s := newConversationsServer(f)

	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{Limit: 100000}))
	c.Require().NoError(err, "ConversationSearch")
	c.Eq(500, f.gotFilter.Limit, "filter Limit")
}

func TestConversationSearchErrorFailsInternalAndRedacts(t *testing.T) {
	c := assert.NewCollecting(t)
	s := newConversationsServer(&fakeConversationInsights{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	c.Require().Eq(connect.CodeInternal, connect.CodeOf(err), "ConversationSearch error err = %v, want", err)
	// The raw error's text must not reach the peer: a pgx failure names the
	// database host, user and database.
	var ce *connect.Error
	c.Require().True(errors.As(err, &ce), "want a *connect.Error, got %T", err)
	c.Eq("internal error; see the daemon log", ce.Message(), "internal error text")
}

// An error the source already coded -- scopeFor's refusal, or a connectapi.ControllerError
// the adapter translated -- must reach the wire under its own code and message,
// never re-wrapped as internal.
func TestConversationSearchPreservesACodedError(t *testing.T) {
	c := assert.NewCollecting(t)
	coded := connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))
	s := newConversationsServer(&fakeConversationInsights{err: coded})
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "coded error err = %v, want", err)
	c.Eq(coded.Error(), err.Error(), "coded error text")
}

func TestConversationExportPreservesACodedError(t *testing.T) {
	c := assert.NewCollecting(t)
	coded := connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))
	s := newConversationsServer(&fakeConversationInsights{err: coded})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "c1"}))
	c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "coded error err = %v, want", err)
	c.Eq(coded.Error(), err.Error(), "coded error text")
}

func TestConversationExportNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "c1"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationExport unwired err = %v, want", err)
}

// Empty id on the EXPORT request — there is no such field on search.
func TestConversationExportEmptyIDFailsInvalidArgument(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{}))
	assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ConversationExport empty id err = %v, want", err)
}

// ok=false must read as not-found: a scope miss and a missing conversation are
// deliberately indistinguishable.
func TestConversationExportOkFalseReadsAsNotFound(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{ok: false})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "someone-elses"}))
	assert.NewAborting(t).Eq(connect.CodeNotFound, connect.CodeOf(err), "ConversationExport ok=false err = %v, want", err)
}

func TestConversationExportMapsTranscript(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{
		ok: true,
		tr: connectapi.TranscriptRow{
			ConversationID: "conv-1", Owner: "brent", Persona: "worker",
			Source: "proxy", DrivenBy: "claude",
			Turns: []connectapi.TranscriptTurnRow{{
				Ordinal: 3, Role: "assistant", Content: []byte(`[{"type":"text"}]`),
				Skills:      []string{"brainstorming"},
				InputTokens: ptrInt64(100), OutputTokens: ptrInt64(20), CacheReadTokens: ptrInt64(80),
				LatencyMS: ptrInt(1500), Model: "openrouter/x/glm", PrefixHash: "abc123",
			}, {
				Ordinal: 4, Role: "user", Content: []byte(`[{"type":"text"}]`),
			}, {
				Ordinal: 5, Role: "assistant", Content: []byte(`[{"type":"text"}]`),
				InputTokens: ptrInt64(0), LatencyMS: ptrInt(0),
			}},
			AvailableSkills: []string{"brainstorming", "writing-plans"},
		},
	}
	s := newConversationsServer(f)

	resp, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "conv-1"}))
	c.Require().NoError(err, "ConversationExport")
	c.Eq("conv-1", f.gotID, "got conversation id")
	msg := resp.Msg
	if msg.GetConversationId() != "conv-1" || msg.GetOwner() != "brent" || msg.GetDrivenBy() != "claude" {
		t.Errorf("header = (%q,%q,%q), want (conv-1,brent,claude)",
			msg.GetConversationId(), msg.GetOwner(), msg.GetDrivenBy())
	}
	c.Require().Len(msg.GetTurns(), 3, "turns = %d, want 3", len(msg.GetTurns()))
	turn := msg.GetTurns()[0]
	if turn.GetOrdinal() != 3 || turn.GetRole() != "assistant" || string(turn.GetContent()) != `[{"type":"text"}]` {
		t.Errorf("turn = (%d,%q,%s), want (3,assistant,[{\"type\":\"text\"}])",
			turn.GetOrdinal(), turn.GetRole(), turn.GetContent())
	}
	if len(turn.GetSkills()) != 1 || turn.GetSkills()[0] != "brainstorming" {
		t.Errorf("turn skills = %v, want [brainstorming]", turn.GetSkills())
	}
	if turn.GetLatencyMs() != 1500 || turn.GetPrefixHash() != "abc123" {
		t.Errorf("turn metrics = (%d,%q), want (1500,abc123)", turn.GetLatencyMs(), turn.GetPrefixHash())
	}
	if turn.InputTokens == nil || turn.GetInputTokens() != 100 || turn.CacheReadTokens == nil {
		t.Errorf("reported metrics must be set on the wire: in=%v cache=%v", turn.InputTokens, turn.CacheReadTokens)
	}
	// Unreported metrics stay UNSET on the wire, not zero.
	unmetered := msg.GetTurns()[1]
	if unmetered.InputTokens != nil || unmetered.OutputTokens != nil ||
		unmetered.CacheReadTokens != nil || unmetered.LatencyMs != nil {
		t.Errorf("unreported metrics must be unset, got in=%v out=%v cache=%v latency=%v",
			unmetered.InputTokens, unmetered.OutputTokens, unmetered.CacheReadTokens, unmetered.LatencyMs)
	}
	// A measured zero stays SET on the wire: zero is not "not reported".
	zeroed := msg.GetTurns()[2]
	if zeroed.InputTokens == nil || zeroed.LatencyMs == nil {
		t.Errorf("measured zeros must stay set, got in=%v latency=%v", zeroed.InputTokens, zeroed.LatencyMs)
	}
	if len(msg.GetAvailableSkills()) != 2 || msg.GetAvailableSkills()[0] != "brainstorming" {
		t.Errorf("available_skills = %v, want [brainstorming writing-plans]", msg.GetAvailableSkills())
	}
}

func TestConversationQueryNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{Name: "tools"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationQuery unwired err = %v, want", err)
}

// Empty name on the QUERY request: the catalogue is addressed by name and no
// default exists.
func TestConversationQueryEmptyNameFailsInvalidArgument(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{})
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{}))
	assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ConversationQuery empty name err = %v, want", err)
}

// A successful query must carry the declared columns through verbatim and
// land each cell in the oneof variant its QueryRowValue flags select.
func TestConversationQueryMapsColumnsAndCellVariants(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationInsights{result: connectapi.CatalogueResult{
		Columns: []connectapi.QueryColumnMeta{
			{Name: "tool", Kind: "string"},
			{Name: "calls", Kind: "int"},
			{Name: "avg_cost", Kind: "float", Format: "usd"},
		},
		Rows: [][]connectapi.QueryRowValue{{
			{Str: "bash"},
			{Int: 42, IsInt: true},
			{Float: 0.125, IsFloat: true},
		}},
	}}
	s := newConversationsServer(f)

	resp, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{
			Name: "tools", SinceUnix: ptrInt64(1757000000), Owner: "brent", Path: "proxy",
		}))
	c.Require().NoError(err, "ConversationQuery")
	c.Eq("tools", f.gotName, "got name")
	if f.gotQuery.Owner != "brent" || f.gotQuery.SinceUnix != 1757000000 || f.gotQuery.Path != "proxy" {
		t.Errorf("filter = %+v, want owner=brent since=1757000000 path=proxy", f.gotQuery)
	}
	cols := resp.Msg.GetColumns()
	c.False(len(cols) != 3 || cols[0].GetName() != "tool" || cols[1].GetKind() != "int" || cols[2].GetFormat() != "usd", "columns = %+v, want (tool,string)(calls,int)(avg_cost,float,usd)", cols)
	rows := resp.Msg.GetRows()
	c.Require().False(len(rows) != 1 || len(rows[0].GetCells()) != 3, "rows = %+v, want one row of three cells", rows)
	cells := rows[0].GetCells()
	if cells[0].GetStrValue() != "bash" || cells[1].GetIntValue() != 42 || cells[2].GetFloatValue() != 0.125 {
		t.Errorf("cells = (%q,%d,%v), want (bash,42,0.125)",
			cells[0].GetStrValue(), cells[1].GetIntValue(), cells[2].GetFloatValue())
	}
	c.False(cells[0].GetV() == nil || cells[1].GetV() == nil || cells[2].GetV() == nil, "oneof not set on every cell: %+v", cells)
}

// An error from the source rides queryError: uncoded becomes a redacted
// internal, exactly like Search and Export.
func TestConversationQueryErrorFailsInternalAndRedacts(t *testing.T) {
	c := assert.NewCollecting(t)
	s := newConversationsServer(&fakeConversationInsights{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{Name: "tools"}))
	c.Require().Eq(connect.CodeInternal, connect.CodeOf(err), "ConversationQuery error err = %v, want", err)
	var ce *connect.Error
	c.Require().True(errors.As(err, &ce), "want a *connect.Error, got %T", err)
	c.Eq("internal error; see the daemon log", ce.Message(), "internal error text")
}
