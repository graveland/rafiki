// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ConversationSearchFilter mirrors insights.SearchFilter. A nil Since/Until is
// unbounded, matching that type's nil-means-unbounded convention.
type ConversationSearchFilter struct {
	Since, Until                                *time.Time
	Owner, Persona, Source, Model, Status, Path string
	MinTokens                                   int64
	Text                                        string
	Limit                                       int
}

// ConversationSummaryRow mirrors insights.ConversationSummary.
type ConversationSummaryRow struct {
	ID, Name, Owner, Persona, Source, Model, Status, DrivenBy string
	CreatedAt                                                 time.Time
	ClosedAt                                                  *time.Time // nil = still open
	Turns                                                     int
	InputTokens, OutputTokens, CacheReadTokens                int64
	CacheHitRatio, TotalCostUSD                               float64
	FirstMessage                                              string
}

// ConversationListSort mirrors insights.ListSort: the row order of a
// conversation listing. The zero value is the recent order, matching both
// that type's and the proto enum's "unspecified = recent" convention.
type ConversationListSort int

const (
	ConversationListSortRecent    ConversationListSort = iota // last activity first
	ConversationListSortCreated                               // creation time first
	ConversationListSortRelevance                             // best match for the query first
)

// ConversationListState mirrors insights.ListState: the lifecycle state of
// the agent linked to a conversation.
type ConversationListState string

const (
	ConversationListStateOpen    ConversationListState = "open"
	ConversationListStateExited  ConversationListState = "exited"
	ConversationListStateClosed  ConversationListState = "closed"
	ConversationListStateNoAgent ConversationListState = "no_agent"
)

// ConversationListFilter mirrors insights.ListFilter field for field. A nil
// Since/Until is unbounded, and every other zero value means "not narrowed",
// matching that type's conventions.
type ConversationListFilter struct {
	Limit                     int
	Cursor                    string
	Sort                      ConversationListSort
	ParentConversationID      string
	Query                     string
	States                    []ConversationListState
	Kinds                     []string
	NameContains, CwdContains string
	Labels                    map[string]string
	Since, Until              *time.Time
}

// ConversationListingRow mirrors insights.ConversationListing.
type ConversationListingRow struct {
	ID, Name, Model, Owner   string
	CreatedAt, LastActivity  time.Time
	ClosedAt                 *time.Time // nil = still open
	Turns                    int
	CostUSD                  float64
	ParentConversationID     string // "" = top level
	ChildCount               int
	ChildID, ChildName, Kind string
	State                    ConversationListState
}

// TranscriptTurnRow mirrors insights.TranscriptTurn.
type TranscriptTurnRow struct {
	Ordinal                                    int
	Role                                       string
	Content                                    []byte
	Skills                                     []string
	InputTokens, OutputTokens, CacheReadTokens *int64         // nil = not reported
	Latency                                    *time.Duration // nil = not reported
	Model, PrefixHash                          string
	ServedProvider                             string // OpenRouter provider that served the turn; empty = not reported
}

// TranscriptRow mirrors insights.Transcript.
type TranscriptRow struct {
	ConversationID, Owner, Persona, Source, DrivenBy string
	Turns                                            []TranscriptTurnRow
	AvailableSkills                                  []string
}

// CatalogueFilter mirrors insights.StatsFilter. Named CatalogueFilter, not
// ConversationSearchFilter, to keep it visibly distinct from that existing
// type -- this is a different filter shape used by a different RPC.
type CatalogueFilter struct {
	Since, Until                        *time.Time
	Owner, Persona, Source, Model, Path string
}

// QueryColumnMeta mirrors insights.Column.
type QueryColumnMeta struct {
	Name, Kind, Format string
}

// QueryRowValue mirrors insights.Entry: exactly one of these three is set,
// matching the field the row's Kind names. A plain Go union (not an
// interface) is enough here -- this type crosses exactly one boundary
// (Controller to proto) and is never stored or passed further, unlike
// insights.Entry which is read by every catalogue query.
type QueryRowValue struct {
	Str     string
	Int     int64
	Float   float64
	IsInt   bool
	IsFloat bool
}

// CatalogueResult mirrors insights.QueryResult.
type CatalogueResult struct {
	Columns []QueryColumnMeta
	Rows    [][]QueryRowValue
}

// ConversationInsights answers scoped conversation reads. The daemon derives
// scope from the caller's own authenticated identity server-side -- no method
// takes one, matching QuotaReader's "no caller-supplied id" shape.
type ConversationInsights interface {
	Search(ctx context.Context, f ConversationSearchFilter) ([]ConversationSummaryRow, error)
	// List answers ListConversations: one level of the conversation tree under
	// the caller's server-derived scope, with the opaque cursor to continue
	// after. The adapter owns the scope AND the excluded-entrypoint list, so no
	// caller choice can re-admit a conversation the daemon's own jobs wrote.
	List(ctx context.Context, f ConversationListFilter) ([]ConversationListingRow, string, error)
	// Visible reports whether conversationID is inside the caller's scope.
	// Unknown and malformed ids answer (false, nil) -- the handler turns both
	// into the one not-found answer, so a scope miss reads exactly like
	// absence.
	Visible(ctx context.Context, conversationID string) (bool, error)
	// Export returns ok=false when the conversation does not exist OR is
	// outside the caller's scope -- the two must be indistinguishable (a
	// scope miss reads exactly like not-found, never a permission error).
	Export(ctx context.Context, conversationID string) (TranscriptRow, bool, error)
	RunQuery(ctx context.Context, name string, f CatalogueFilter) (CatalogueResult, error)
}

// SetConversationInsights attaches the conversation-query source.
// Post-construction setter for the same reason as SetChildLister.
func (s *Server) SetConversationInsights(c ConversationInsights) { s.conversations.Store(&c) }

const maxConversationSearchLimit = 500

func (s *Server) ConversationSearch(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationSearchRequest],
) (*connect.Response[rafikiv1.ConversationSearchResponse], error) {
	p := s.conversations.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation insights not yet wired"))
	}
	limit := int(req.Msg.GetLimit())
	if limit > maxConversationSearchLimit {
		limit = maxConversationSearchLimit
	}
	since, err := protoTimePtr(req.Msg.GetSince())
	if err != nil {
		return nil, err
	}
	until, err := protoTimePtr(req.Msg.GetUntil())
	if err != nil {
		return nil, err
	}
	f := ConversationSearchFilter{
		Since: since, Until: until,
		Owner: req.Msg.GetOwner(), Persona: req.Msg.GetPersona(), Source: req.Msg.GetSource(),
		Model: req.Msg.GetModel(), Status: req.Msg.GetStatus(), Path: req.Msg.GetPath(),
		MinTokens: req.Msg.GetMinTokens(), Text: req.Msg.GetText(), Limit: limit,
	}
	rows, err := (*p).Search(ctx, f)
	if err != nil {
		return nil, queryError(err)
	}
	out := make([]*rafikiv1.ConversationSummary, 0, len(rows))
	for _, r := range rows {
		row := &rafikiv1.ConversationSummary{
			Id: r.ID, Name: r.Name, Owner: r.Owner, Persona: r.Persona, Source: r.Source,
			Model: r.Model, Status: r.Status, DrivenBy: r.DrivenBy, CreatedAt: timestamppb.New(r.CreatedAt),
			Turns: int32(r.Turns), InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheHitRatio: r.CacheHitRatio,
			TotalCostUsd: r.TotalCostUSD, FirstMessage: r.FirstMessage,
		}
		// ClosedAt stays unset (never a zero Timestamp) for an open conversation,
		// so "closed at the epoch" and "still open" cannot collapse.
		if r.ClosedAt != nil {
			row.ClosedAt = timestamppb.New(*r.ClosedAt)
		}
		out = append(out, row)
	}
	return connect.NewResponse(&rafikiv1.ConversationSearchResponse{Rows: out}), nil
}

// listSortFromProto maps the wire sort enum onto the filter's sort. The
// enum's UNSPECIFIED and the filter's zero value both mean the recent order,
// so the two zero conventions collapse here once, at the boundary.
func listSortFromProto(v rafikiv1.ConversationSort) ConversationListSort {
	switch v {
	case rafikiv1.ConversationSort_CONVERSATION_SORT_CREATED:
		return ConversationListSortCreated
	case rafikiv1.ConversationSort_CONVERSATION_SORT_RELEVANCE:
		return ConversationListSortRelevance
	default:
		return ConversationListSortRecent
	}
}

// listStatesFromProto maps the wire state enums onto the filter's states. An
// UNSPECIFIED entry inside the list is refused: it is not a state, and
// silently dropping it would widen the answer past what the caller asked to
// see.
func listStatesFromProto(vals []rafikiv1.ConversationState) ([]ConversationListState, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := make([]ConversationListState, 0, len(vals))
	for _, v := range vals {
		switch v {
		case rafikiv1.ConversationState_CONVERSATION_STATE_OPEN:
			out = append(out, ConversationListStateOpen)
		case rafikiv1.ConversationState_CONVERSATION_STATE_EXITED:
			out = append(out, ConversationListStateExited)
		case rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED:
			out = append(out, ConversationListStateClosed)
		case rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT:
			out = append(out, ConversationListStateNoAgent)
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("states contains an unspecified value"))
		}
	}
	return out, nil
}

// ListConversations serves one level of the conversation tree under the
// caller's server-derived scope. The wire carries a filter, never a scope or
// an exclusion list: the adapter derives the scope from the caller's own
// identity and always excludes the conversations the daemon's own LLM jobs
// wrote, so no request can widen either.
func (s *Server) ListConversations(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListConversationsRequest],
) (*connect.Response[rafikiv1.ListConversationsResponse], error) {
	p := s.conversations.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation insights not yet wired"))
	}
	limit := int(req.Msg.GetLimit())
	if limit < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("limit must not be negative"))
	}
	if limit > maxConversationSearchLimit {
		limit = maxConversationSearchLimit
	}
	since, err := protoTimePtr(req.Msg.GetSince())
	if err != nil {
		return nil, err
	}
	until, err := protoTimePtr(req.Msg.GetUntil())
	if err != nil {
		return nil, err
	}
	states, err := listStatesFromProto(req.Msg.GetStates())
	if err != nil {
		return nil, err
	}
	f := ConversationListFilter{
		Limit: limit, Cursor: req.Msg.GetCursor(),
		Sort:                 listSortFromProto(req.Msg.GetSort()),
		ParentConversationID: req.Msg.GetParentConversationId(),
		Query:                req.Msg.GetQuery(), States: states, Kinds: req.Msg.GetKinds(),
		NameContains: req.Msg.GetNameContains(), CwdContains: req.Msg.GetCwdContains(),
		Labels: req.Msg.GetLabels(), Since: since, Until: until,
	}
	// The adapter codes every error it can mean for the caller (a bad cursor,
	// a relevance sort without a query, content search this database cannot
	// serve, a credential refusal); the handler adds no mapping of its own.
	rows, next, err := (*p).List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]*rafikiv1.ConversationListing, 0, len(rows))
	for _, r := range rows {
		row := &rafikiv1.ConversationListing{
			Id: r.ID, Name: r.Name, Model: r.Model, Owner: r.Owner,
			CreatedAt: timestamppb.New(r.CreatedAt), LastActivity: timestamppb.New(r.LastActivity),
			Turns: int32(r.Turns), CostUsd: r.CostUSD,
			ParentConversationId: r.ParentConversationID, ChildCount: int32(r.ChildCount),
			ChildId: r.ChildID, ChildName: r.ChildName, Kind: r.Kind,
			State: listingStateToProto(r.State),
		}
		// ClosedAt stays unset (never a zero Timestamp) for an open
		// conversation, so "closed at the epoch" and "still open" cannot
		// collapse.
		if r.ClosedAt != nil {
			row.ClosedAt = timestamppb.New(*r.ClosedAt)
		}
		out = append(out, row)
	}
	return connect.NewResponse(&rafikiv1.ListConversationsResponse{Rows: out, NextCursor: next}), nil
}

// listingStateToProto maps the filter-side state back onto the wire enum. The
// filter type's values are exactly the four named states, so every value the
// adapter can return has an arm here.
func listingStateToProto(v ConversationListState) rafikiv1.ConversationState {
	switch v {
	case ConversationListStateOpen:
		return rafikiv1.ConversationState_CONVERSATION_STATE_OPEN
	case ConversationListStateExited:
		return rafikiv1.ConversationState_CONVERSATION_STATE_EXITED
	case ConversationListStateClosed:
		return rafikiv1.ConversationState_CONVERSATION_STATE_CLOSED
	case ConversationListStateNoAgent:
		return rafikiv1.ConversationState_CONVERSATION_STATE_NO_AGENT
	default:
		return rafikiv1.ConversationState_CONVERSATION_STATE_UNSPECIFIED
	}
}

func (s *Server) ConversationExport(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationExportRequest],
) (*connect.Response[rafikiv1.ConversationExportResponse], error) {
	p := s.conversations.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation insights not yet wired"))
	}
	id := req.Msg.GetConversationId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id is required"))
	}
	tr, ok, err := (*p).Export(ctx, id)
	if err != nil {
		return nil, queryError(err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
	}
	turns := make([]*rafikiv1.TranscriptTurn, 0, len(tr.Turns))
	for _, t := range tr.Turns {
		turn := &rafikiv1.TranscriptTurn{
			Ordinal: int32(t.Ordinal), Role: t.Role, Content: t.Content, Skills: t.Skills,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
			Model: t.Model, PrefixHash: t.PrefixHash,
			ServedProvider: t.ServedProvider,
		}
		if t.Latency != nil {
			turn.Latency = durationpb.New(*t.Latency)
		}
		turns = append(turns, turn)
	}
	return connect.NewResponse(&rafikiv1.ConversationExportResponse{
		ConversationId: tr.ConversationID, Owner: tr.Owner, Persona: tr.Persona,
		Source: tr.Source, DrivenBy: tr.DrivenBy, Turns: turns, AvailableSkills: tr.AvailableSkills,
	}), nil
}

func (s *Server) ConversationQuery(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationQueryRequest],
) (*connect.Response[rafikiv1.ConversationQueryResponse], error) {
	p := s.conversations.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation insights not yet wired"))
	}
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	since, err := protoTimePtr(req.Msg.GetSince())
	if err != nil {
		return nil, err
	}
	until, err := protoTimePtr(req.Msg.GetUntil())
	if err != nil {
		return nil, err
	}
	f := CatalogueFilter{
		Since: since, Until: until,
		Owner: req.Msg.GetOwner(), Persona: req.Msg.GetPersona(), Source: req.Msg.GetSource(),
		Model: req.Msg.GetModel(), Path: req.Msg.GetPath(),
	}
	res, err := (*p).RunQuery(ctx, name, f)
	if err != nil {
		return nil, queryError(err)
	}
	cols := make([]*rafikiv1.QueryColumn, 0, len(res.Columns))
	for _, c := range res.Columns {
		cols = append(cols, &rafikiv1.QueryColumn{Name: c.Name, Kind: c.Kind, Format: c.Format})
	}
	rows := make([]*rafikiv1.QueryRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		cells := make([]*rafikiv1.QueryValue, 0, len(r))
		for _, v := range r {
			switch {
			case v.IsInt:
				cells = append(cells, &rafikiv1.QueryValue{V: &rafikiv1.QueryValue_IntValue{IntValue: v.Int}})
			case v.IsFloat:
				cells = append(cells, &rafikiv1.QueryValue{V: &rafikiv1.QueryValue_FloatValue{FloatValue: v.Float}})
			default:
				cells = append(cells, &rafikiv1.QueryValue{V: &rafikiv1.QueryValue_StrValue{StrValue: v.Str}})
			}
		}
		rows = append(rows, &rafikiv1.QueryRow{Cells: cells})
	}
	return connect.NewResponse(&rafikiv1.ConversationQueryResponse{Columns: cols, Rows: rows}), nil
}

// internalRedactedText is what a genuinely uncoded error says on the wire.
// Its raw text is logged, never forwarded: a pgx failure names the database
// host, user and database, which a caller has no business learning from a
// failed request. Mirrors pkg/control's mapErr, whose comment explains the
// allowlist discipline -- an error that must reach the caller is promoted to
// a curated error at its source (the *ControllerError the daemon's
// adapter translates), and everything else is redacted by default. This
// package deliberately does not import pkg/control to inspect that type: it
// imports pkg/insights, which this package must never reach, so the
// curated-error translation lives in the adapter layer instead.
const internalRedactedText = "internal error; see the daemon log"

// queryError passes an already-coded error through untouched -- scopeFor's
// CodePermissionDenied refusal must reach the wire as permission_denied, not
// be re-wrapped into internal -- and redacts everything else. A *connect.Error
// was built by code that chose its code deliberately; any other error is
// infrastructure text this package did not author.
func queryError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	slog.Error("connect: conversation query failed", "error", err)
	return connect.NewError(connect.CodeInternal, errors.New(internalRedactedText))
}

// protoTimePtr converts an optional wire Timestamp to *time.Time, treating an
// unset message AND the epoch (the message's zero value) as nil -- the
// filter's "unbounded", matching the old integer 0. An out-of-range value is
// refused as InvalidArgument, per design rule 4.
func protoTimePtr(ts *timestamppb.Timestamp) (*time.Time, error) {
	if ts == nil {
		return nil, nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if ts.GetSeconds() == 0 && ts.GetNanos() == 0 {
		return nil, nil
	}
	t := ts.AsTime()
	return &t, nil
}
