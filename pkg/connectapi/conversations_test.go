// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeConversationInsights struct {
	gotFilter connectapi.ConversationSearchFilter
	gotID     string
	gotName   string
	gotQuery  connectapi.CatalogueFilter

	searchCalled bool
	rows         []connectapi.ConversationSummaryRow
	tr           connectapi.TranscriptRow
	ok           bool
	err          error

	result connectapi.CatalogueResult
}

func (f *fakeConversationInsights) Search(_ context.Context, flt connectapi.ConversationSearchFilter) ([]connectapi.ConversationSummaryRow, error) {
	f.searchCalled = true
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

// ptrMS builds the *time.Duration a turn's latency takes.
func ptrMS(ms int) *time.Duration {
	d := time.Duration(ms) * time.Millisecond
	return &d
}

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
		CreatedAt: time.Unix(1757000000, 0), Turns: 7,
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

// TestConversationSearchMapsClosedAtRow pins the row's closed_at on the wire: a
// set value appears as a Timestamp, an absent one stays unset (never a zero
// Timestamp), so "closed" and "still open" cannot collapse.
func TestConversationSearchMapsClosedAtRow(t *testing.T) {
	c := assert.NewCollecting(t)
	closed := time.Unix(1757000000, 0)
	f := &fakeConversationInsights{rows: []connectapi.ConversationSummaryRow{
		{ID: "closed", ClosedAt: &closed},
		{ID: "open"},
	}}
	s := newConversationsServer(f)

	resp, err := s.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	c.Require().NoError(err, "ConversationSearch")

	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 2, "rows = %d, want 2", len(rows))
	c.Require().True(rows[0].ClosedAt != nil, "closed row must set closed_at on the wire")
	c.Eq(closed.Unix(), rows[0].GetClosedAt().AsTime().Unix(), "closed_at")
	c.False(rows[1].ClosedAt != nil, "open row must leave closed_at unset, got %v", rows[1].GetClosedAt())
}

// TestConversationSearchRequestHasNoClosedField pins the removal: the request
// message must carry no `closed` field and its old number 12 must be reserved,
// so no client can send the removed filter.
func TestConversationSearchRequestHasNoClosedField(t *testing.T) {
	md := (&rafikiv1.ConversationSearchRequest{}).ProtoReflect().Descriptor()
	if fd := md.Fields().ByName(protoreflect.Name("closed")); fd != nil {
		t.Errorf("ConversationSearchRequest still has a `closed` field: %v", fd)
	}
	if fd := md.Fields().ByNumber(12); fd != nil {
		t.Errorf("field number 12 is still live: %v", fd)
	}
	if !md.ReservedNames().Has("closed") {
		t.Errorf("name `closed` is not reserved")
	}
	if !md.ReservedRanges().Has(12) {
		t.Errorf("number 12 is not reserved: %v", md.ReservedRanges())
	}
}

// TestConversationSearchSinceUntilRoundTrip pins that a set since/until reach
// the filter as times, and that unset stays nil (unbounded).
func TestConversationSearchSinceUntilRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	since := time.Unix(1700000000, 0)
	until := time.Unix(1700003600, 0)
	f := &fakeConversationInsights{}
	s := newConversationsServer(f)

	_, err := s.ConversationSearch(context.Background(), connect.NewRequest(&rafikiv1.ConversationSearchRequest{
		Since: timestamppb.New(since), Until: timestamppb.New(until),
	}))
	c.Require().NoError(err, "ConversationSearch")
	c.Require().True(f.gotFilter.Since != nil && f.gotFilter.Until != nil, "since/until reached the seam")
	c.Eq(since.Unix(), f.gotFilter.Since.Unix(), "since")
	c.Eq(until.Unix(), f.gotFilter.Until.Unix(), "until")
}

func TestConversationSearchUnsetOrEpochSinceIsUnbounded(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.ConversationSearchRequest
	}{
		{"unset", &rafikiv1.ConversationSearchRequest{}},
		{"epoch", &rafikiv1.ConversationSearchRequest{Since: timestamppb.New(time.Unix(0, 0))}},
	} {
		f := &fakeConversationInsights{}
		_, err := newConversationsServer(f).ConversationSearch(context.Background(), connect.NewRequest(tc.req))
		c.Require().NoError(err, "%s: ConversationSearch", tc.name)
		c.False(f.gotFilter.Since != nil, "%s: since must stay nil", tc.name)
	}
}

// An out-of-range Timestamp is refused InvalidArgument before the adapter runs.
func TestConversationSearchRejectsOutOfRangeTimes(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.ConversationSearchRequest
	}{
		{"since", &rafikiv1.ConversationSearchRequest{Since: outOfRangeTimestamp}},
		{"until", &rafikiv1.ConversationSearchRequest{Until: outOfRangeTimestamp}},
	} {
		f := &fakeConversationInsights{}
		_, err := newConversationsServer(f).ConversationSearch(context.Background(), connect.NewRequest(tc.req))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", tc.name)
		c.False(f.searchCalled, "%s: the adapter must not be called", tc.name)
	}
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
				Latency: ptrMS(1500), Model: "openrouter/x/glm", PrefixHash: "abc123",
			}, {
				Ordinal: 4, Role: "user", Content: []byte(`[{"type":"text"}]`),
			}, {
				Ordinal: 5, Role: "assistant", Content: []byte(`[{"type":"text"}]`),
				InputTokens: ptrInt64(0), Latency: ptrMS(0),
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
	if got := turn.GetLatency(); got == nil || got.AsDuration() != 1500*time.Millisecond || turn.GetPrefixHash() != "abc123" {
		t.Errorf("turn metrics = (%v,%q), want (1.5s,abc123)", turn.GetLatency(), turn.GetPrefixHash())
	}
	if turn.InputTokens == nil || turn.GetInputTokens() != 100 || turn.CacheReadTokens == nil {
		t.Errorf("reported metrics must be set on the wire: in=%v cache=%v", turn.InputTokens, turn.CacheReadTokens)
	}
	// Unreported metrics stay UNSET on the wire, not zero.
	unmetered := msg.GetTurns()[1]
	if unmetered.InputTokens != nil || unmetered.OutputTokens != nil ||
		unmetered.CacheReadTokens != nil || unmetered.Latency != nil {
		t.Errorf("unreported metrics must be unset, got in=%v out=%v cache=%v latency=%v",
			unmetered.InputTokens, unmetered.OutputTokens, unmetered.CacheReadTokens, unmetered.Latency)
	}
	// A measured zero stays SET on the wire: zero is not "not reported".
	zeroed := msg.GetTurns()[2]
	if zeroed.InputTokens == nil || zeroed.Latency == nil {
		t.Errorf("measured zeros must stay set, got in=%v latency=%v", zeroed.InputTokens, zeroed.Latency)
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

// An out-of-range Timestamp on the query request is refused before the adapter.
func TestConversationQueryRejectsOutOfRangeTimes(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		req  *rafikiv1.ConversationQueryRequest
	}{
		{"since", &rafikiv1.ConversationQueryRequest{Name: "tools", Since: outOfRangeTimestamp}},
		{"until", &rafikiv1.ConversationQueryRequest{Name: "tools", Until: outOfRangeTimestamp}},
	} {
		_, err := newConversationsServer(&fakeConversationInsights{}).ConversationQuery(context.Background(), connect.NewRequest(tc.req))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", tc.name)
	}
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
			Name: "tools", Since: timestamppb.New(time.Unix(1757000000, 0)), Owner: "brent", Path: "proxy",
		}))
	c.Require().NoError(err, "ConversationQuery")
	c.Eq("tools", f.gotName, "got name")
	if f.gotQuery.Owner != "brent" || f.gotQuery.Since == nil || f.gotQuery.Since.Unix() != 1757000000 || f.gotQuery.Path != "proxy" {
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
