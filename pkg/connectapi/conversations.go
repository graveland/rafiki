// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

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

// ConversationInsights answers scoped conversation reads. The daemon derives
// scope from the caller's own authenticated identity server-side -- neither
// method takes one, matching QuotaReader's "no caller-supplied id" shape.
type ConversationInsights interface {
	Search(ctx context.Context, f ConversationSearchFilter) ([]ConversationSummaryRow, error)
	// Export returns ok=false when the conversation does not exist OR is
	// outside the caller's scope -- the two must be indistinguishable (a
	// scope miss reads exactly like not-found, never a permission error).
	Export(ctx context.Context, conversationID string) (TranscriptRow, bool, error)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
