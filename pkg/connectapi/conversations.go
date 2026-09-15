// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ConversationSearchFilter mirrors insights.SearchFilter. SinceUnix/UntilUnix
// of 0 mean unset, matching that type's nil-means-unbounded convention.
type ConversationSearchFilter struct {
	SinceUnix, UntilUnix                        int64
	Owner, Persona, Source, Model, Status, Path string
	MinTokens                                   int64
	Text                                        string
	Limit                                       int
}

// ConversationSummaryRow mirrors insights.ConversationSummary.
type ConversationSummaryRow struct {
	ID, Name, Owner, Persona, Source, Model, Status, DrivenBy string
	CreatedAtUnix                                             int64
	Turns                                                     int
	InputTokens, OutputTokens, CacheReadTokens                int64
	CacheHitRatio, TotalCostUSD                               float64
	FirstMessage                                              string
}

// TranscriptTurnRow mirrors insights.TranscriptTurn.
type TranscriptTurnRow struct {
	Ordinal                                    int
	Role                                       string
	Content                                    []byte
	Skills                                     []string
	InputTokens, OutputTokens, CacheReadTokens int64
	LatencyMS                                  int
	Model, PrefixHash                          string
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
	SinceUnix, UntilUnix                int64
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
	f := ConversationSearchFilter{
		SinceUnix: req.Msg.GetSinceUnix(), UntilUnix: req.Msg.GetUntilUnix(),
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
		out = append(out, &rafikiv1.ConversationSummary{
			Id: r.ID, Name: r.Name, Owner: r.Owner, Persona: r.Persona, Source: r.Source,
			Model: r.Model, Status: r.Status, DrivenBy: r.DrivenBy, CreatedAtUnix: r.CreatedAtUnix,
			Turns: int32(r.Turns), InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheHitRatio: r.CacheHitRatio,
			TotalCostUsd: r.TotalCostUSD, FirstMessage: r.FirstMessage,
		})
	}
	return connect.NewResponse(&rafikiv1.ConversationSearchResponse{Rows: out}), nil
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
		turns = append(turns, &rafikiv1.TranscriptTurn{
			Ordinal: int32(t.Ordinal), Role: t.Role, Content: t.Content, Skills: t.Skills,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
			LatencyMs: int32(t.LatencyMS), Model: t.Model, PrefixHash: t.PrefixHash,
		})
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
	f := CatalogueFilter{
		SinceUnix: req.Msg.GetSinceUnix(), UntilUnix: req.Msg.GetUntilUnix(),
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
// a curated error at its source (the *control.ControllerError the daemon's
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
