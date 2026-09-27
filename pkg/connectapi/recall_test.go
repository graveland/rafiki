// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/recall"

	"github.com/multigres/testkit/assert"
)

type fakeRecall struct {
	hits   []recall.Hit
	text   string
	mem    recall.Memory
	tree   []recall.Memory
	status RecallStatus

	recallErr   error
	getErr      error
	treeErr     error
	putErr      error
	delErr      error
	backfillErr error
	statusErr   error

	gotQuery recall.SearchQuery
	gotLimit int
	gotPut   recall.Memory
	recalls  int
	puts     int
}

func (f *fakeRecall) Recall(_ context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error) {
	f.recalls++
	f.gotQuery, f.gotLimit = q, limit
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	return f.hits, nil
}

func (f *fakeRecall) RecallContext(_ context.Context, _ string, _, _, _ int) (string, error) {
	return f.text, nil
}

func (f *fakeRecall) GetMemory(_ context.Context, _, _ string) (recall.Memory, error) {
	if f.getErr != nil {
		return recall.Memory{}, f.getErr
	}
	return f.mem, nil
}

func (f *fakeRecall) MemoryTree(_ context.Context, _ string, _ int) ([]recall.Memory, error) {
	if f.treeErr != nil {
		return nil, f.treeErr
	}
	return f.tree, nil
}

func (f *fakeRecall) PutMemory(_ context.Context, m recall.Memory) (recall.Memory, error) {
	f.puts++
	f.gotPut = m
	if f.putErr != nil {
		return recall.Memory{}, f.putErr
	}
	return f.mem, nil
}

func (f *fakeRecall) DeleteMemory(_ context.Context, _, _ string) error { return f.delErr }

func (f *fakeRecall) Backfill(_ context.Context, _ time.Time, _ float64) error {
	return f.backfillErr
}

func (f *fakeRecall) Status(_ context.Context) (RecallStatus, error) {
	if f.statusErr != nil {
		return RecallStatus{}, f.statusErr
	}
	return f.status, nil
}

// TestSetRecallManagerNilIsRefused pins the nil-refusal rule: SetRecallManager(nil)
// stores nothing, so the next call fails closed with Unavailable instead of
// nil-panicking inside the handler.
func TestSetRecallManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetRecallManager(nil)
	_, err := s.Recall(context.Background(), connect.NewRequest(&rafikiv1.RecallRequest{Query: "q"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetRecallManager(nil): got code")
}

func TestRecallUnavailableWhenUnwired(t *testing.T) {
	s := &Server{}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"Recall", func() error {
			_, err := s.Recall(context.Background(), connect.NewRequest(&rafikiv1.RecallRequest{Query: "q"}))
			return err
		}},
		{"RecallContext", func() error {
			_, err := s.RecallContext(context.Background(), connect.NewRequest(&rafikiv1.RecallContextRequest{Id: "s:x"}))
			return err
		}},
		{"GetMemory", func() error {
			_, err := s.GetMemory(context.Background(), connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: "a", Name: "b"}))
			return err
		}},
		{"MemoryTree", func() error {
			_, err := s.MemoryTree(context.Background(), connect.NewRequest(&rafikiv1.MemoryTreeRequest{Path: "a"}))
			return err
		}},
		{"PutMemory", func() error {
			_, err := s.PutMemory(context.Background(), connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "a", Name: "b"}))
			return err
		}},
		{"DeleteMemory", func() error {
			_, err := s.DeleteMemory(context.Background(), connect.NewRequest(&rafikiv1.DeleteMemoryRequest{Path: "a", Name: "b"}))
			return err
		}},
		{"RecallBackfill", func() error {
			_, err := s.RecallBackfill(context.Background(), connect.NewRequest(&rafikiv1.RecallBackfillRequest{}))
			return err
		}},
		{"RecallStatus", func() error {
			_, err := s.RecallStatus(context.Background(), connect.NewRequest(&rafikiv1.RecallStatusRequest{}))
			return err
		}},
	} {
		err := call.fn()
		if err == nil {
			t.Errorf("%s with no manager: accepted", call.name)
			continue
		}
		assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "%s with no manager: got code %v, want Unavailable", call.name, connect.CodeOf(err))
	}
}

// TestRecallErrorMapping pins recallError over the store's four sentinels
// (wrapped, so errors.Is is what carries the mapping) plus the fallback: an
// unknown error is a store failure, CodeInternal.
func TestRecallErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		fn      func(*Server, *fakeRecall, error) error
		want    connect.Code
		wantErr error
	}{
		{
			name:    "not found",
			err:     fmt.Errorf("get: %w", recall.ErrNotFound),
			want:    connect.CodeNotFound,
			wantErr: recall.ErrNotFound,
			fn: func(s *Server, f *fakeRecall, mgrErr error) error {
				f.getErr = mgrErr
				_, err := s.GetMemory(context.Background(),
					connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: "a", Name: "b"}))
				return err
			},
		},
		{
			name:    "invalid path",
			err:     fmt.Errorf("put: %w", recall.ErrInvalidPath),
			want:    connect.CodeInvalidArgument,
			wantErr: recall.ErrInvalidPath,
			fn: func(s *Server, f *fakeRecall, mgrErr error) error {
				f.putErr = mgrErr
				_, err := s.PutMemory(context.Background(),
					connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "bad path", Name: "b"}))
				return err
			},
		},
		{
			name:    "no owner",
			err:     fmt.Errorf("put: %w", recall.ErrNoOwner),
			want:    connect.CodeInvalidArgument,
			wantErr: recall.ErrNoOwner,
			fn: func(s *Server, f *fakeRecall, mgrErr error) error {
				f.putErr = mgrErr
				_, err := s.PutMemory(context.Background(),
					connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "a", Name: "b"}))
				return err
			},
		},
		{
			name:    "invalid scope",
			err:     fmt.Errorf("search: %w", recall.ErrInvalidScope),
			want:    connect.CodePermissionDenied,
			wantErr: recall.ErrInvalidScope,
			fn: func(s *Server, f *fakeRecall, mgrErr error) error {
				f.recallErr = mgrErr
				_, err := s.Recall(context.Background(),
					connect.NewRequest(&rafikiv1.RecallRequest{Query: "q"}))
				return err
			},
		},
		{
			name: "store failure",
			err:  errors.New("connection refused"),
			want: connect.CodeInternal,
			fn: func(s *Server, f *fakeRecall, mgrErr error) error {
				f.getErr = mgrErr
				_, err := s.GetMemory(context.Background(),
					connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: "a", Name: "b"}))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			s := &Server{}
			f := &fakeRecall{}
			s.SetRecallManager(f)
			err := tc.fn(s, f, tc.err)
			c.Require().Error(err, "accepted")
			c.Eq(tc.want, connect.CodeOf(err), "got code")
			c.False(tc.wantErr != nil && !errors.Is(err, tc.wantErr), "error does not wrap %v: %v", tc.wantErr, err)
		})
	}
}

func TestRecallLimitClamp(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct{ wire, want int }{
		{0, recall.RecallDefaultLimit},
		{1, 1},
		{30, 30},
		{recall.RecallMaxLimit, recall.RecallMaxLimit},
		{51, recall.RecallMaxLimit},
		{500, recall.RecallMaxLimit},
	} {
		s := &Server{}
		f := &fakeRecall{}
		s.SetRecallManager(f)
		_, err := s.Recall(context.Background(),
			connect.NewRequest(&rafikiv1.RecallRequest{Query: "q", Limit: int32(tc.wire)}))
		c.Require().NoError(err, "limit %d", tc.wire)
		c.Eq(tc.want, f.gotLimit, "limit %d arrived as %d, want", tc.wire, f.gotLimit)
	}
}

// TestRecallMapsRequestToSearchQuery pins the wire conversion: query, source
// filter, path/repo filters and unix-second bounds land on the SearchQuery the
// manager receives (Scope/MemoryOwner left for the adapter), and hits come
// back with RFC3339 UTC timestamps.
func TestRecallMapsRequestToSearchQuery(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRecall{hits: []recall.Hit{{
		ID: "s:abc", Source: recall.SourceSummary, Snippet: "snip",
		When:             time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ConversationID:   "conv",
		ConversationName: "name",
		Repo:             "rafiki",
		Kind:             "fundi",
		OrdinalFrom:      3,
		OrdinalTo:        9,
		Title:            "t",
		Score:            0.42,
	}}}
	s.SetRecallManager(f)

	since := int64(1700000000)
	until := int64(1700001000)
	resp, err := s.Recall(context.Background(), connect.NewRequest(&rafikiv1.RecallRequest{
		Query:     "needle",
		Sources:   []string{"memory", "window"},
		Under:     "projects",
		Repo:      "rafiki",
		SinceUnix: since,
		UntilUnix: until,
	}))
	c.Require().NoError(err, "recall")

	q := f.gotQuery
	c.False(q.Text != "needle" || q.Under != "projects" || q.Repo != "rafiki", "search query text/under/repo = %q/%q/%q", q.Text, q.Under, q.Repo)
	c.False(len(q.Sources) != 2 || q.Sources[0] != recall.SourceMemory || q.Sources[1] != recall.SourceWindow, "sources = %v, want [memory window]", q.Sources)
	c.False(q.Scope != (recall.Scope{}) || q.MemoryOwner != "", "scope/owner resolved by handler: %+v/%q, want zero (adapter's job)", q.Scope, q.MemoryOwner)
	c.False(q.Since == nil || !q.Since.Equal(time.Unix(since, 0).UTC()), "since = %v, want unix %d", q.Since, since)
	c.False(q.Until == nil || !q.Until.Equal(time.Unix(until, 0).UTC()), "until = %v, want unix %d", q.Until, until)

	c.Require().Len(resp.Msg.Hits, 1, "got %d hits, want 1", len(resp.Msg.Hits))
	h := resp.Msg.Hits[0]
	if h.GetId() != "s:abc" || h.GetSource() != "summary" || h.GetSnippet() != "snip" {
		t.Errorf("hit id/source/snippet = %q/%q/%q", h.GetId(), h.GetSource(), h.GetSnippet())
	}
	c.Eq("2026-01-02T03:04:05Z", h.GetWhen(), "when")
	c.False(h.GetConversationId() != "conv" || h.GetConversationName() != "name" ||
		h.GetRepo() != "rafiki" || h.GetKind() != "fundi" ||
		h.GetOrdinalFrom() != 3 || h.GetOrdinalTo() != 9 ||
		h.GetTitle() != "t" || h.GetScore() != 0.42, "hit fields drifted: %+v", h)
}

// TestRecallUnknownSourceRejected pins the source filter validation: an
// unrecognized name is a bad request, not a silent no-match.
func TestRecallUnknownSourceRejected(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRecall{}
	s.SetRecallManager(f)
	_, err := s.Recall(context.Background(), connect.NewRequest(&rafikiv1.RecallRequest{
		Query:   "q",
		Sources: []string{"memory", "bogus"},
	}))
	c.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "unknown source: got code")
	c.Eq(0, f.recalls, "manager reached despite the rejection")
}

// TestRecallPutMemoryMetaJson pins meta_json handling: absent means "{}",
// invalid JSON is a bad request that never reaches the manager, valid JSON is
// passed through byte-for-byte.
func TestRecallPutMemoryMetaJson(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRecall{mem: recall.Memory{Path: "a", Name: "b", Meta: json.RawMessage(`{"k":1}`)}}
	s.SetRecallManager(f)

	if _, err := s.PutMemory(context.Background(),
		connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "a", Name: "b", Body: "x"})); err != nil {
		t.Fatalf("put without meta_json: %v", err)
	}
	c.Eq("{}", string(f.gotPut.Meta), "absent meta_json arrived as")

	if _, err := s.PutMemory(context.Background(),
		connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "a", Name: "b", MetaJson: "{oops"})); err == nil {
		t.Fatalf("put with invalid meta_json: accepted")
	} else if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid meta_json: got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	c.Eq(1, f.puts, "manager reached")

	resp, err := s.PutMemory(context.Background(),
		connect.NewRequest(&rafikiv1.PutMemoryRequest{Path: "a", Name: "b", MetaJson: `{"k":1}`}))
	c.Require().NoError(err, "put with meta_json")
	c.Eq(`{"k":1}`, string(f.gotPut.Meta), "meta_json drifted")
	c.Eq(`{"k":1}`, resp.Msg.GetMemory().GetMetaJson(), "response meta_json =")
}

// TestRecallStatusMapsFields pins the status conversion, including the two
// shapes of backfill_since: RFC3339 UTC when backfill is on, empty when off.
func TestRecallStatusMapsFields(t *testing.T) {
	c := assert.NewCollecting(t)
	since := time.Unix(1700000000, 0).UTC()
	s := &Server{}
	s.SetRecallManager(&fakeRecall{status: RecallStatus{
		Status: recall.Status{
			Conversations:     3,
			Windows:           7,
			WindowsUnembedded: 2,
			Summaries:         5,
			SummariesPending:  1,
			Memories:          9,
			SummaryCostUSD:    0.25,
			BackfillSince:     &since,
			BackfillBudgetUSD: 2,
			BackfillSpentUSD:  0.5,
			EmbeddingModel:    "emb",
		},
		SummaryModel: "sum",
	}})
	resp, err := s.RecallStatus(context.Background(), connect.NewRequest(&rafikiv1.RecallStatusRequest{}))
	c.Require().NoError(err, "status")
	m := resp.Msg
	c.False(m.GetConversations() != 3 || m.GetWindows() != 7 || m.GetWindowsUnembedded() != 2 ||
		m.GetSummaries() != 5 || m.GetSummariesPending() != 1 || m.GetMemories() != 9, "counts drifted: %+v", m)
	c.False(m.GetSummaryCostUsd() != 0.25 || m.GetBackfillBudgetUsd() != 2 || m.GetBackfillSpentUsd() != 0.5, "costs drifted: %+v", m)
	c.Eq("2023-11-14T22:13:20Z", m.GetBackfillSince(), "backfill_since")
	if m.GetEmbeddingModel() != "emb" || m.GetSummaryModel() != "sum" {
		t.Errorf("models drifted: %q/%q", m.GetEmbeddingModel(), m.GetSummaryModel())
	}

	s = &Server{}
	s.SetRecallManager(&fakeRecall{})
	resp, err = s.RecallStatus(context.Background(), connect.NewRequest(&rafikiv1.RecallStatusRequest{}))
	c.Require().NoError(err, "status without backfill")
	c.Eq("", resp.Msg.GetBackfillSince(), "backfill_since with no backfill")
}
