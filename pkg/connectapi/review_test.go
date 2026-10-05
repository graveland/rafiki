// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeConversationReviewer struct {
	gotReq connectapi.ReviewRequest

	accepts []connectapi.ReviewAccept
	err     error
}

func (f *fakeConversationReviewer) Review(_ context.Context, req connectapi.ReviewRequest) ([]connectapi.ReviewAccept, error) {
	f.gotReq = req
	return f.accepts, f.err
}

type fakeConversationFindingsReader struct {
	gotFilter         connectapi.ReviewFindingsFilter
	gotAnalysisIDs    []string
	gotAnalysisLimit  int
	analysesCallCount int

	findings []connectapi.ReviewFinding
	analyses []connectapi.ReviewAnalysis
	err      error
}

func (f *fakeConversationFindingsReader) Findings(_ context.Context, flt connectapi.ReviewFindingsFilter) ([]connectapi.ReviewFinding, error) {
	f.gotFilter = flt
	return f.findings, f.err
}

func (f *fakeConversationFindingsReader) RecentAnalyses(_ context.Context, conversationIDs []string, limit int) ([]connectapi.ReviewAnalysis, error) {
	f.gotAnalysisIDs = conversationIDs
	f.gotAnalysisLimit = limit
	f.analysesCallCount++
	return f.analyses, f.err
}

func newReviewServer(r *fakeConversationReviewer) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetConversationReviewer(r)
	return s
}

func newFindingsServer(r *fakeConversationFindingsReader) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetConversationFindingsReader(r)
	return s
}

func TestConversationReviewNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationReview(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationReviewRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationReview unwired err = %v, want", err)
}

// ptrDouble/ptrInt32 build the pointer forms a proto `optional` field decodes
// to, so the test can distinguish "unset" from a zero value.
func ptrDouble(v float64) *float64 { return &v }
func ptrInt32(v int32) *int32      { return &v }
func ptrBool(v bool) *bool         { return &v }
func ptrString(v string) *string   { return &v }

func TestConversationReviewMapsRequestAndAccepts(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationReviewer{accepts: []connectapi.ReviewAccept{
		{ConversationID: "conv-1", Status: "enqueued"},
		{ConversationID: "conv-2", Status: "already_running"},
		{ConversationID: "conv-3", Status: "queue_full"},
	}}
	s := newReviewServer(f)

	resp, err := s.ConversationReview(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationReviewRequest{
			ConversationIds: []string{"conv-1", "conv-2", "conv-3"},
			Stage:           rafikiv1.ReviewStage_REVIEW_STAGE_RANK,
			Model:           ptrString("openrouter/x/glm"),
			Profile:         ptrString("work"),
			BudgetUsd:       ptrDouble(0.50),
			MinTurns:        ptrInt32(4),
			Force:           ptrBool(true),
		}))
	c.Require().NoError(err, "ConversationReview")
	r := f.gotReq
	c.False(len(r.ConversationIDs) != 3 || r.ConversationIDs[1] != "conv-2", "ConversationIDs = %v, want [conv-1 conv-2 conv-3]", r.ConversationIDs)
	c.Eq("rank", r.Stage, "Stage")
	c.False(r.Model != "openrouter/x/glm" || r.Profile != "work", "model/profile = (%q,%q), want (openrouter/x/glm,work)", r.Model, r.Profile)
	c.False(!r.HasBudgetUSD || r.BudgetUSD != 0.50, "budget = (%v,%v), want (true,0.5)", r.HasBudgetUSD, r.BudgetUSD)
	c.False(!r.HasMinTurns || r.MinTurns != 4, "min_turns = (%v,%v), want (true,4)", r.HasMinTurns, r.MinTurns)
	c.True(r.Force, "Force = false, want true")
	got := resp.Msg.GetAccepted()
	c.Require().Len(got, 3, "accepted = %d, want 3", len(got))
	want := []rafikiv1.ReviewAcceptStatus{
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING,
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL,
	}
	for i, a := range got {
		c.Eq(f.accepts[i].ConversationID, a.GetConversationId(), "accepted[%d].conversation_id = %q, want", i, a.GetConversationId())
		c.Eq(want[i], a.GetStatus(), "accepted[%d].status = %v, want", i, a.GetStatus())
	}
}

// UNSPECIFIED (the zero value an omitted field decodes to) and DETECT both
// map to "detect"; only RANK is "rank".
func TestConversationReviewStageDefaultsToDetect(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		stage rafikiv1.ReviewStage
		want  string
	}{
		{rafikiv1.ReviewStage_REVIEW_STAGE_UNSPECIFIED, "detect"},
		{rafikiv1.ReviewStage_REVIEW_STAGE_DETECT, "detect"},
		{rafikiv1.ReviewStage_REVIEW_STAGE_RANK, "rank"},
	} {
		f := &fakeConversationReviewer{}
		s := newReviewServer(f)
		_, err := s.ConversationReview(context.Background(),
			connect.NewRequest(&rafikiv1.ConversationReviewRequest{Stage: tc.stage}))
		c.Require().NoError(err, "ConversationReview stage %v", tc.stage)
		c.Eq(tc.want, f.gotReq.Stage, "stage %v -> %q, want", tc.stage, f.gotReq.Stage)
	}
}

func TestConversationReviewErrorFailsInternalAndRedacts(t *testing.T) {
	c := assert.NewCollecting(t)
	s := newReviewServer(&fakeConversationReviewer{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationReview(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationReviewRequest{ConversationIds: []string{"conv-1"}}))
	c.Require().Eq(connect.CodeInternal, connect.CodeOf(err), "ConversationReview error err = %v, want", err)
	// The raw error's text must not reach the peer: a pgx failure names the
	// database host, user and database.
	var ce *connect.Error
	c.Require().True(errors.As(err, &ce), "want a *connect.Error, got %T", err)
	c.Eq("internal error; see the daemon log", ce.Message(), "internal error text")
}

func TestConversationFindingsNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationFindings unwired err = %v, want", err)
}

func TestConversationFindingsMapsRowsAndFilter(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationFindingsReader{
		findings: []connectapi.ReviewFinding{{
			ID: "f-1", AnalysisID: "a-1", ConversationID: "conv-1",
			Axis: "tooling", TopicKey: "bash-loop", SkillName: "writing-plans",
			Title: "repeated bash polling", ExpectedSavingsTokens: 4200, Status: "open",
		}},
		analyses: []connectapi.ReviewAnalysis{{
			ID: "a-1", ConversationID: "conv-1", Model: "openrouter/x/glm", Profile: "work",
			Status: "ok", Error: "",
			InputTokens: 15000, OutputTokens: 900, CostUSD: 0.0210, CreatedAt: time.Unix(1757000000, 0),
		}},
	}
	s := newFindingsServer(f)

	resp, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{
			Axis: "tooling", Skill: "writing-plans", Status: "resolved",
			ConversationIds: []string{"conv-1"}, Limit: 25,
		}))
	c.Require().NoError(err, "ConversationFindings")
	if f.gotFilter.Axis != "tooling" || f.gotFilter.Skill != "writing-plans" || f.gotFilter.Status != "resolved" {
		t.Errorf("filter = %+v, want axis=tooling skill=writing-plans status=resolved", f.gotFilter)
	}
	if len(f.gotFilter.ConversationIDs) != 1 || f.gotFilter.ConversationIDs[0] != "conv-1" {
		t.Errorf("filter ConversationIDs = %v, want [conv-1]", f.gotFilter.ConversationIDs)
	}
	c.Eq(25, f.gotFilter.Limit, "filter Limit")
	if f.gotAnalysisLimit != 25 || len(f.gotAnalysisIDs) != 1 {
		t.Errorf("RecentAnalyses args = (%v,%d), want ([conv-1],25)", f.gotAnalysisIDs, f.gotAnalysisLimit)
	}
	got := resp.Msg.GetFindings()
	c.Require().Len(got, 1, "findings = %d, want 1", len(got))
	fd := got[0]
	c.False(fd.GetId() != "f-1" || fd.GetAnalysisId() != "a-1" || fd.GetConversationId() != "conv-1" ||
		fd.GetAxis() != "tooling" || fd.GetTopicKey() != "bash-loop" ||
		fd.GetSkillName() != "writing-plans" || fd.GetTitle() != "repeated bash polling", "finding = %+v, want the fake's row", fd)
	if fd.GetExpectedSavingsTokens() != 4200 || fd.GetStatus() != "open" {
		t.Errorf("finding scalars = (%d,%q), want (4200,open)", fd.GetExpectedSavingsTokens(), fd.GetStatus())
	}
	ana := resp.Msg.GetAnalyses()
	c.Require().Len(ana, 1, "analyses = %d, want 1", len(ana))
	a := ana[0]
	c.False(a.GetId() != "a-1" || a.GetConversationId() != "conv-1" || a.GetModel() != "openrouter/x/glm" ||
		a.GetProfile() != "work" || a.GetStatus() != "ok" || a.GetError() != "", "analysis = %+v, want the fake's row", a)
	if a.GetInputTokens() != 15000 || a.GetOutputTokens() != 900 || a.GetCostUsd() != 0.0210 || a.GetCreatedAt().AsTime().Unix() != 1757000000 {
		t.Errorf("analysis scalars = (%d,%d,%v,%d), want (15000,900,0.021,1757000000)",
			a.GetInputTokens(), a.GetOutputTokens(), a.GetCostUsd(), a.GetCreatedAt().AsTime().Unix())
	}
}

// Status "" must arrive as "" (meaning "open"), not be rewritten.
func TestConversationFindingsEmptyStatusPassesThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeConversationFindingsReader{}
	s := newFindingsServer(f)
	_, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{Axis: "tooling"}))
	c.Require().NoError(err, "ConversationFindings")
	c.Eq("", f.gotFilter.Status, "filter Status")
}

func TestConversationFindingsErrorFailsInternalAndRedacts(t *testing.T) {
	c := assert.NewCollecting(t)
	s := newFindingsServer(&fakeConversationFindingsReader{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{Axis: "tooling"}))
	c.Require().Eq(connect.CodeInternal, connect.CodeOf(err), "ConversationFindings error err = %v, want", err)
	var ce *connect.Error
	c.Require().True(errors.As(err, &ce), "want a *connect.Error, got %T", err)
	c.Eq("internal error; see the daemon log", ce.Message(), "internal error text")
}
