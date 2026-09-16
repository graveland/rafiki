// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ReviewRequest carries one review submission. Stage is "detect" (the
// default, also for an empty string) or "rank". The Has* flags preserve the
// proto optional fields: an unset budget_usd means "no ceiling requested",
// distinct from an explicit 0.
type ReviewRequest struct {
	ConversationIDs []string
	Stage           string // "detect" (default, also empty string) | "rank"
	Model           string
	Profile         string
	BudgetUSD       float64
	HasBudgetUSD    bool
	MinTurns        int32
	HasMinTurns     bool
	Force           bool
}

// ReviewAccept is one conversation's acceptance outcome.
type ReviewAccept struct {
	ConversationID string
	Status         string // "enqueued" | "already_running" | "queue_full"
}

// ConversationReviewer accepts a review request per conversation_id. Scope
// is derived server-side from the caller's credential (matching
// ConversationInsights) -- Review must never receive a scope on the wire and
// must drop an out-of-scope id from its returned slice rather than erroring.
// Review must never block on an LLM call.
type ConversationReviewer interface {
	Review(ctx context.Context, req ReviewRequest) ([]ReviewAccept, error)
}

// SetConversationReviewer attaches the review submission source.
// Post-construction setter for the same reason as SetConversationInsights.
func (s *Server) SetConversationReviewer(r ConversationReviewer) { s.reviewer.Store(&r) }

// ReviewFindingsFilter narrows a findings read. Status "" means "open",
// matching store.ListFindings.
type ReviewFindingsFilter struct {
	Axis, Skill, Status string
	ConversationIDs     []string
	Limit               int
}

// ReviewFinding mirrors one analysis_finding row.
type ReviewFinding struct {
	ID, AnalysisID, ConversationID, Axis, TopicKey, SkillName, Title string
	ExpectedSavingsTokens                                            int64
	Status                                                           string
}

// ReviewAnalysis mirrors one conversation_analysis row.
type ReviewAnalysis struct {
	ID, ConversationID, Model, Profile, Status, Error string
	InputTokens, OutputTokens                         int64
	CostUSD                                           float64
	CreatedAtUnix                                     int64
}

// ConversationFindingsReader answers scoped review reads, matching
// ConversationInsights' "no caller-supplied scope" shape.
type ConversationFindingsReader interface {
	Findings(ctx context.Context, f ReviewFindingsFilter) ([]ReviewFinding, error)
	RecentAnalyses(ctx context.Context, conversationIDs []string, limit int) ([]ReviewAnalysis, error)
}

// SetConversationFindingsReader attaches the findings read source.
// Post-construction setter for the same reason as SetConversationInsights.
func (s *Server) SetConversationFindingsReader(r ConversationFindingsReader) {
	s.findingsReader.Store(&r)
}

// reviewStageToString maps the proto enum onto the store's stage spelling.
// REVIEW_STAGE_UNSPECIFIED defaults to "detect" -- it is the proto zero value,
// so an omitted field is indistinguishable from an explicit
// REVIEW_STAGE_DETECT.
func reviewStageToString(stage rafikiv1.ReviewStage) string {
	if stage == rafikiv1.ReviewStage_REVIEW_STAGE_RANK {
		return "rank"
	}
	return "detect"
}

var reviewAcceptStatusFromString = map[string]rafikiv1.ReviewAcceptStatus{
	"enqueued":        rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	"already_running": rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING,
	"queue_full":      rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL,
}

func (s *Server) ConversationReview(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationReviewRequest],
) (*connect.Response[rafikiv1.ConversationReviewResponse], error) {
	p := s.reviewer.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation reviewer not yet wired"))
	}
	r := ReviewRequest{
		ConversationIDs: req.Msg.GetConversationIds(),
		Stage:           reviewStageToString(req.Msg.GetStage()),
		Model:           req.Msg.GetModel(),
		Profile:         req.Msg.GetProfile(),
		HasBudgetUSD:    req.Msg.BudgetUsd != nil,
		MinTurns:        req.Msg.GetMinTurns(),
		HasMinTurns:     req.Msg.MinTurns != nil,
		Force:           req.Msg.GetForce(),
	}
	if req.Msg.BudgetUsd != nil {
		r.BudgetUSD = req.Msg.GetBudgetUsd()
	}
	accepts, err := (*p).Review(ctx, r)
	if err != nil {
		return nil, queryError(err)
	}
	out := make([]*rafikiv1.ConversationReviewAccept, 0, len(accepts))
	for _, a := range accepts {
		status := rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_UNSPECIFIED
		if st, ok := reviewAcceptStatusFromString[a.Status]; ok {
			status = st
		}
		out = append(out, &rafikiv1.ConversationReviewAccept{
			ConversationId: a.ConversationID, Status: status,
		})
	}
	return connect.NewResponse(&rafikiv1.ConversationReviewResponse{Accepted: out}), nil
}

func (s *Server) ConversationFindings(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationFindingsRequest],
) (*connect.Response[rafikiv1.ConversationFindingsResponse], error) {
	p := s.findingsReader.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("conversation findings not yet wired"))
	}
	f := ReviewFindingsFilter{
		Axis: req.Msg.GetAxis(), Skill: req.Msg.GetSkill(), Status: req.Msg.GetStatus(),
		ConversationIDs: req.Msg.GetConversationIds(), Limit: int(req.Msg.GetLimit()),
	}
	findings, err := (*p).Findings(ctx, f)
	if err != nil {
		return nil, queryError(err)
	}
	analyses, err := (*p).RecentAnalyses(ctx, req.Msg.GetConversationIds(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, queryError(err)
	}
	outFindings := make([]*rafikiv1.ReviewFinding, 0, len(findings))
	for _, r := range findings {
		outFindings = append(outFindings, &rafikiv1.ReviewFinding{
			Id: r.ID, AnalysisId: r.AnalysisID, ConversationId: r.ConversationID,
			Axis: r.Axis, TopicKey: r.TopicKey, SkillName: r.SkillName, Title: r.Title,
			ExpectedSavingsTokens: r.ExpectedSavingsTokens, Status: r.Status,
		})
	}
	outAnalyses := make([]*rafikiv1.ReviewAnalysis, 0, len(analyses))
	for _, a := range analyses {
		outAnalyses = append(outAnalyses, &rafikiv1.ReviewAnalysis{
			Id: a.ID, ConversationId: a.ConversationID, Model: a.Model, Profile: a.Profile,
			Status: a.Status, Error: a.Error,
			InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
			CostUsd: a.CostUSD, CreatedAtUnix: a.CreatedAtUnix,
		})
	}
	return connect.NewResponse(&rafikiv1.ConversationFindingsResponse{
		Findings: outFindings, Analyses: outAnalyses,
	}), nil
}
