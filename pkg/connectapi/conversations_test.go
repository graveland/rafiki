// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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

func TestConversationSearchNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ConversationSearch unwired err = %v, want %v", err, connect.CodeUnavailable)
	}
}

func TestConversationSearchMapsRowsAndFilter(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ConversationSearch: %v", err)
	}
	if f.gotFilter.Owner != "brent" {
		t.Errorf("filter Owner = %q, want brent", f.gotFilter.Owner)
	}
	if f.gotFilter.Limit != 20 {
		t.Errorf("filter Limit = %d, want 20", f.gotFilter.Limit)
	}
	got := resp.Msg.GetRows()
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].GetId() != "c1" {
		t.Errorf("Id = %q, want c1", got[0].GetId())
	}
	if got[0].GetOwner() != "brent" {
		t.Errorf("Owner = %q, want brent", got[0].GetOwner())
	}
	if got[0].GetTotalCostUsd() != 0.0123 {
		t.Errorf("TotalCostUsd = %v, want 0.0123", got[0].GetTotalCostUsd())
	}
}

// The server clamps a caller's limit; it never forwards an unbounded one.
func TestConversationSearchClampsLimit(t *testing.T) {
	f := &fakeConversationInsights{}
	s := newConversationsServer(f)

	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{Limit: 100000}))
	if err != nil {
		t.Fatalf("ConversationSearch: %v", err)
	}
	if f.gotFilter.Limit != 500 {
		t.Errorf("filter Limit = %d, want clamped to 500", f.gotFilter.Limit)
	}
}

func TestConversationSearchErrorFailsInternalAndRedacts(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("ConversationSearch error err = %v, want %v", err, connect.CodeInternal)
	}
	// The raw error's text must not reach the peer: a pgx failure names the
	// database host, user and database.
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a *connect.Error, got %T", err)
	}
	if msg := ce.Message(); msg != "internal error; see the daemon log" {
		t.Errorf("internal error text = %q, want the mapErr redaction", msg)
	}
}

// An error the source already coded -- scopeFor's refusal, or a ControllerError
// the adapter translated -- must reach the wire under its own code and message,
// never re-wrapped as internal.
func TestConversationSearchPreservesACodedError(t *testing.T) {
	coded := connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))
	s := newConversationsServer(&fakeConversationInsights{err: coded})
	_, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("coded error err = %v, want %v", err, connect.CodePermissionDenied)
	}
	if err.Error() != coded.Error() {
		t.Errorf("coded error text = %q, want %q", err.Error(), coded.Error())
	}
}

func TestConversationExportPreservesACodedError(t *testing.T) {
	coded := connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))
	s := newConversationsServer(&fakeConversationInsights{err: coded})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "c1"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("coded error err = %v, want %v", err, connect.CodePermissionDenied)
	}
	if err.Error() != coded.Error() {
		t.Errorf("coded error text = %q, want %q", err.Error(), coded.Error())
	}
}

func TestConversationExportNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "c1"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ConversationExport unwired err = %v, want %v", err, connect.CodeUnavailable)
	}
}

// Empty id on the EXPORT request — there is no such field on search.
func TestConversationExportEmptyIDFailsInvalidArgument(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ConversationExport empty id err = %v, want %v", err, connect.CodeInvalidArgument)
	}
}

// ok=false must read as not-found: a scope miss and a missing conversation are
// deliberately indistinguishable.
func TestConversationExportOkFalseReadsAsNotFound(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{ok: false})
	_, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "someone-elses"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("ConversationExport ok=false err = %v, want %v", err, connect.CodeNotFound)
	}
}

func TestConversationExportMapsTranscript(t *testing.T) {
	f := &fakeConversationInsights{
		ok: true,
		tr: connectapi.TranscriptRow{
			ConversationID: "conv-1", Owner: "brent", Persona: "worker",
			Source: "proxy", DrivenBy: "claude",
			Turns: []connectapi.TranscriptTurnRow{{
				Ordinal: 3, Role: "assistant", Content: []byte(`[{"type":"text"}]`),
				Skills:      []string{"brainstorming"},
				InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80,
				LatencyMS: 1500, Model: "openrouter/x/glm", PrefixHash: "abc123",
			}},
			AvailableSkills: []string{"brainstorming", "writing-plans"},
		},
	}
	s := newConversationsServer(f)

	resp, err := s.ConversationExport(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "conv-1"}))
	if err != nil {
		t.Fatalf("ConversationExport: %v", err)
	}
	if f.gotID != "conv-1" {
		t.Errorf("got conversation id %q, want conv-1", f.gotID)
	}
	msg := resp.Msg
	if msg.GetConversationId() != "conv-1" || msg.GetOwner() != "brent" || msg.GetDrivenBy() != "claude" {
		t.Errorf("header = (%q,%q,%q), want (conv-1,brent,claude)",
			msg.GetConversationId(), msg.GetOwner(), msg.GetDrivenBy())
	}
	if len(msg.GetTurns()) != 1 {
		t.Fatalf("turns = %d, want 1", len(msg.GetTurns()))
	}
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
	if len(msg.GetAvailableSkills()) != 2 || msg.GetAvailableSkills()[0] != "brainstorming" {
		t.Errorf("available_skills = %v, want [brainstorming writing-plans]", msg.GetAvailableSkills())
	}
}

func TestConversationQueryNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{Name: "tools"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ConversationQuery unwired err = %v, want %v", err, connect.CodeUnavailable)
	}
}

// Empty name on the QUERY request: the catalogue is addressed by name and no
// default exists.
func TestConversationQueryEmptyNameFailsInvalidArgument(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{})
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ConversationQuery empty name err = %v, want %v", err, connect.CodeInvalidArgument)
	}
}

// A successful query must carry the declared columns through verbatim and
// land each cell in the oneof variant its QueryRowValue flags select.
func TestConversationQueryMapsColumnsAndCellVariants(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ConversationQuery: %v", err)
	}
	if f.gotName != "tools" {
		t.Errorf("got name %q, want tools", f.gotName)
	}
	if f.gotQuery.Owner != "brent" || f.gotQuery.SinceUnix != 1757000000 || f.gotQuery.Path != "proxy" {
		t.Errorf("filter = %+v, want owner=brent since=1757000000 path=proxy", f.gotQuery)
	}
	cols := resp.Msg.GetColumns()
	if len(cols) != 3 || cols[0].GetName() != "tool" || cols[1].GetKind() != "int" || cols[2].GetFormat() != "usd" {
		t.Errorf("columns = %+v, want (tool,string)(calls,int)(avg_cost,float,usd)", cols)
	}
	rows := resp.Msg.GetRows()
	if len(rows) != 1 || len(rows[0].GetCells()) != 3 {
		t.Fatalf("rows = %+v, want one row of three cells", rows)
	}
	cells := rows[0].GetCells()
	if cells[0].GetStrValue() != "bash" || cells[1].GetIntValue() != 42 || cells[2].GetFloatValue() != 0.125 {
		t.Errorf("cells = (%q,%d,%v), want (bash,42,0.125)",
			cells[0].GetStrValue(), cells[1].GetIntValue(), cells[2].GetFloatValue())
	}
	if cells[0].GetV() == nil || cells[1].GetV() == nil || cells[2].GetV() == nil {
		t.Errorf("oneof not set on every cell: %+v", cells)
	}
}

// An error from the source rides queryError: uncoded becomes a redacted
// internal, exactly like Search and Export.
func TestConversationQueryErrorFailsInternalAndRedacts(t *testing.T) {
	s := newConversationsServer(&fakeConversationInsights{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationQuery(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationQueryRequest{Name: "tools"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("ConversationQuery error err = %v, want %v", err, connect.CodeInternal)
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a *connect.Error, got %T", err)
	}
	if msg := ce.Message(); msg != "internal error; see the daemon log" {
		t.Errorf("internal error text = %q, want the mapErr redaction", msg)
	}
}
