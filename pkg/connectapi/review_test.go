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
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ConversationReview unwired err = %v, want %v", err, connect.CodeUnavailable)
	}
}

// ptrDouble/ptrInt32 build the pointer forms a proto `optional` field decodes
// to, so the test can distinguish "unset" from a zero value.
func ptrDouble(v float64) *float64 { return &v }
func ptrInt32(v int32) *int32      { return &v }
func ptrBool(v bool) *bool         { return &v }
func ptrString(v string) *string   { return &v }

func TestConversationReviewMapsRequestAndAccepts(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ConversationReview: %v", err)
	}
	r := f.gotReq
	if len(r.ConversationIDs) != 3 || r.ConversationIDs[1] != "conv-2" {
		t.Errorf("ConversationIDs = %v, want [conv-1 conv-2 conv-3]", r.ConversationIDs)
	}
	if r.Stage != "rank" {
		t.Errorf("Stage = %q, want rank", r.Stage)
	}
	if r.Model != "openrouter/x/glm" || r.Profile != "work" {
		t.Errorf("model/profile = (%q,%q), want (openrouter/x/glm,work)", r.Model, r.Profile)
	}
	if !r.HasBudgetUSD || r.BudgetUSD != 0.50 {
		t.Errorf("budget = (%v,%v), want (true,0.5)", r.HasBudgetUSD, r.BudgetUSD)
	}
	if !r.HasMinTurns || r.MinTurns != 4 {
		t.Errorf("min_turns = (%v,%v), want (true,4)", r.HasMinTurns, r.MinTurns)
	}
	if !r.Force {
		t.Errorf("Force = false, want true")
	}
	got := resp.Msg.GetAccepted()
	if len(got) != 3 {
		t.Fatalf("accepted = %d, want 3", len(got))
	}
	want := []rafikiv1.ReviewAcceptStatus{
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING,
		rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL,
	}
	for i, a := range got {
		if a.GetConversationId() != f.accepts[i].ConversationID {
			t.Errorf("accepted[%d].conversation_id = %q, want %q", i, a.GetConversationId(), f.accepts[i].ConversationID)
		}
		if a.GetStatus() != want[i] {
			t.Errorf("accepted[%d].status = %v, want %v", i, a.GetStatus(), want[i])
		}
	}
}

// UNSPECIFIED (the zero value an omitted field decodes to) and DETECT both
// map to "detect"; only RANK is "rank".
func TestConversationReviewStageDefaultsToDetect(t *testing.T) {
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
		if _, err := s.ConversationReview(context.Background(),
			connect.NewRequest(&rafikiv1.ConversationReviewRequest{Stage: tc.stage})); err != nil {
			t.Fatalf("ConversationReview stage %v: %v", tc.stage, err)
		}
		if f.gotReq.Stage != tc.want {
			t.Errorf("stage %v -> %q, want %q", tc.stage, f.gotReq.Stage, tc.want)
		}
	}
}

func TestConversationReviewErrorFailsInternalAndRedacts(t *testing.T) {
	s := newReviewServer(&fakeConversationReviewer{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationReview(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationReviewRequest{ConversationIds: []string{"conv-1"}}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("ConversationReview error err = %v, want %v", err, connect.CodeInternal)
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

func TestConversationFindingsNotWiredFailsUnavailable(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ConversationFindings unwired err = %v, want %v", err, connect.CodeUnavailable)
	}
}

func TestConversationFindingsMapsRowsAndFilter(t *testing.T) {
	f := &fakeConversationFindingsReader{
		findings: []connectapi.ReviewFinding{{
			ID: "f-1", AnalysisID: "a-1", ConversationID: "conv-1",
			Axis: "tooling", TopicKey: "bash-loop", SkillName: "writing-plans",
			Title: "repeated bash polling", ExpectedSavingsTokens: 4200, Status: "open",
		}},
		analyses: []connectapi.ReviewAnalysis{{
			ID: "a-1", ConversationID: "conv-1", Model: "openrouter/x/glm", Profile: "work",
			Status: "ok", Error: "",
			InputTokens: 15000, OutputTokens: 900, CostUSD: 0.0210, CreatedAtUnix: 1757000000,
		}},
	}
	s := newFindingsServer(f)

	resp, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{
			Axis: "tooling", Skill: "writing-plans", Status: "resolved",
			ConversationIds: []string{"conv-1"}, Limit: 25,
		}))
	if err != nil {
		t.Fatalf("ConversationFindings: %v", err)
	}
	if f.gotFilter.Axis != "tooling" || f.gotFilter.Skill != "writing-plans" || f.gotFilter.Status != "resolved" {
		t.Errorf("filter = %+v, want axis=tooling skill=writing-plans status=resolved", f.gotFilter)
	}
	if len(f.gotFilter.ConversationIDs) != 1 || f.gotFilter.ConversationIDs[0] != "conv-1" {
		t.Errorf("filter ConversationIDs = %v, want [conv-1]", f.gotFilter.ConversationIDs)
	}
	if f.gotFilter.Limit != 25 {
		t.Errorf("filter Limit = %d, want 25", f.gotFilter.Limit)
	}
	if f.gotAnalysisLimit != 25 || len(f.gotAnalysisIDs) != 1 {
		t.Errorf("RecentAnalyses args = (%v,%d), want ([conv-1],25)", f.gotAnalysisIDs, f.gotAnalysisLimit)
	}
	got := resp.Msg.GetFindings()
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	fd := got[0]
	if fd.GetId() != "f-1" || fd.GetAnalysisId() != "a-1" || fd.GetConversationId() != "conv-1" ||
		fd.GetAxis() != "tooling" || fd.GetTopicKey() != "bash-loop" ||
		fd.GetSkillName() != "writing-plans" || fd.GetTitle() != "repeated bash polling" {
		t.Errorf("finding = %+v, want the fake's row", fd)
	}
	if fd.GetExpectedSavingsTokens() != 4200 || fd.GetStatus() != "open" {
		t.Errorf("finding scalars = (%d,%q), want (4200,open)", fd.GetExpectedSavingsTokens(), fd.GetStatus())
	}
	ana := resp.Msg.GetAnalyses()
	if len(ana) != 1 {
		t.Fatalf("analyses = %d, want 1", len(ana))
	}
	a := ana[0]
	if a.GetId() != "a-1" || a.GetConversationId() != "conv-1" || a.GetModel() != "openrouter/x/glm" ||
		a.GetProfile() != "work" || a.GetStatus() != "ok" || a.GetError() != "" {
		t.Errorf("analysis = %+v, want the fake's row", a)
	}
	if a.GetInputTokens() != 15000 || a.GetOutputTokens() != 900 || a.GetCostUsd() != 0.0210 || a.GetCreatedAtUnix() != 1757000000 {
		t.Errorf("analysis scalars = (%d,%d,%v,%d), want (15000,900,0.021,1757000000)",
			a.GetInputTokens(), a.GetOutputTokens(), a.GetCostUsd(), a.GetCreatedAtUnix())
	}
}

// Status "" must arrive as "" (meaning "open"), not be rewritten.
func TestConversationFindingsEmptyStatusPassesThrough(t *testing.T) {
	f := &fakeConversationFindingsReader{}
	s := newFindingsServer(f)
	if _, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{Axis: "tooling"})); err != nil {
		t.Fatalf("ConversationFindings: %v", err)
	}
	if f.gotFilter.Status != "" {
		t.Errorf("filter Status = %q, want empty", f.gotFilter.Status)
	}
}

func TestConversationFindingsErrorFailsInternalAndRedacts(t *testing.T) {
	s := newFindingsServer(&fakeConversationFindingsReader{err: errors.New("db down: host=db.internal user=rafiki")})
	_, err := s.ConversationFindings(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationFindingsRequest{Axis: "tooling"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("ConversationFindings error err = %v, want %v", err, connect.CodeInternal)
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a *connect.Error, got %T", err)
	}
	if msg := ce.Message(); msg != "internal error; see the daemon log" {
		t.Errorf("internal error text = %q, want the mapErr redaction", msg)
	}
}
