// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/recall"
)

// RecallManager serves recall and memories for the calling identity; the
// adapter resolves scope/owner from the request context. SearchQuery arrives
// with Scope and MemoryOwner unset for exactly that reason — filling them is
// the implementation's job, and its failure to do so reads back as
// ErrInvalidScope / ErrNoOwner, not as a handler bug.
type RecallManager interface {
	Recall(ctx context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error)
	RecallContext(ctx context.Context, id string, before, after, maxChars int) (string, error)
	GetMemory(ctx context.Context, path, name string) (recall.Memory, error)
	MemoryTree(ctx context.Context, path string, depth int) ([]recall.Memory, error)
	PutMemory(ctx context.Context, m recall.Memory) (recall.Memory, error)
	DeleteMemory(ctx context.Context, path, name string) error
	Backfill(ctx context.Context, since time.Time, maxCostUSD float64) error
	Status(ctx context.Context) (RecallStatus, error)
}

// RecallStatus is the store's Status plus the summarizer's model, which the
// store does not know (it only persists what the summarizer used per call).
type RecallStatus struct {
	recall.Status
	SummaryModel string
}

// SetRecallManager attaches the recall backend. A nil manager is refused
// rather than stored (see SetPresetManager): storing &m for a nil interface
// would defeat recallManager's Unavailable path and nil-panic the first
// handler call instead.
func (s *Server) SetRecallManager(m RecallManager) {
	if m == nil {
		return
	}
	s.recall.Store(&m)
}

func (s *Server) recallManager() (RecallManager, error) {
	r := s.recall.Load()
	if r == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("recall backend not yet wired"))
	}
	return *r, nil
}

// recallError maps a manager failure onto a Connect code: an unknown row is
// CodeNotFound, a bad memory path or a missing owner is the caller's fault
// (CodeInvalidArgument), a scope that admits nothing is CodePermissionDenied,
// and everything else — a store failure — is CodeInternal rather than
// something that reads as the caller's fault.
func recallError(err error) error {
	switch {
	case errors.Is(err, recall.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, recall.ErrInvalidPath), errors.Is(err, recall.ErrNoOwner):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, recall.ErrInvalidScope):
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (s *Server) Recall(
	ctx context.Context, req *connect.Request[rafikiv1.RecallRequest],
) (*connect.Response[rafikiv1.RecallResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetQuery() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}
	sources, err := recallSources(req.Msg.GetSources())
	if err != nil {
		return nil, err
	}
	q := recall.SearchQuery{
		Text:    req.Msg.GetQuery(),
		Sources: sources,
		Under:   req.Msg.GetUnder(),
		Repo:    req.Msg.GetRepo(),
		Since:   unixTime(req.Msg.GetSinceUnix()),
		Until:   unixTime(req.Msg.GetUntilUnix()),
	}
	hits, err := m.Recall(ctx, q, clampRecallLimit(int(req.Msg.GetLimit())))
	if err != nil {
		return nil, recallError(err)
	}
	out := make([]*rafikiv1.RecallHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, recallHit(h))
	}
	return connect.NewResponse(&rafikiv1.RecallResponse{Hits: out}), nil
}

// recallSources validates the request's source filter. An unrecognized name
// is rejected here rather than passed on, where it would either error deep in
// the store or silently match nothing depending on the source.
func recallSources(names []string) ([]recall.Source, error) {
	if len(names) == 0 {
		return nil, nil
	}
	sources := make([]recall.Source, 0, len(names))
	for _, n := range names {
		switch src := recall.Source(n); src {
		case recall.SourceMemory, recall.SourceSummary, recall.SourceWindow:
			sources = append(sources, src)
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("unknown source %q", n))
		}
	}
	return sources, nil
}

// clampRecallLimit forces the wire limit into [1, RecallMaxLimit], mapping 0
// to the default.
func clampRecallLimit(limit int) int {
	switch {
	case limit <= 0:
		return recall.RecallDefaultLimit
	case limit > recall.RecallMaxLimit:
		return recall.RecallMaxLimit
	}
	return limit
}

// unixTime converts a unix-seconds wire field; 0 means unbounded.
func unixTime(unix int64) *time.Time {
	if unix == 0 {
		return nil
	}
	t := time.Unix(unix, 0).UTC()
	return &t
}

// rfc3339 renders t for the wire: RFC3339 in UTC.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (s *Server) RecallContext(
	ctx context.Context, req *connect.Request[rafikiv1.RecallContextRequest],
) (*connect.Response[rafikiv1.RecallContextResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	text, err := m.RecallContext(ctx, req.Msg.GetId(),
		int(req.Msg.GetBefore()), int(req.Msg.GetAfter()), int(req.Msg.GetMaxChars()))
	if err != nil {
		return nil, recallError(err)
	}
	return connect.NewResponse(&rafikiv1.RecallContextResponse{Text: text}), nil
}

func (s *Server) GetMemory(
	ctx context.Context, req *connect.Request[rafikiv1.GetMemoryRequest],
) (*connect.Response[rafikiv1.GetMemoryResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	mem, err := m.GetMemory(ctx, req.Msg.GetPath(), req.Msg.GetName())
	if err != nil {
		return nil, recallError(err)
	}
	return connect.NewResponse(&rafikiv1.GetMemoryResponse{Memory: memoryRow(mem)}), nil
}

func (s *Server) MemoryTree(
	ctx context.Context, req *connect.Request[rafikiv1.MemoryTreeRequest],
) (*connect.Response[rafikiv1.MemoryTreeResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.MemoryTree(ctx, req.Msg.GetPath(), int(req.Msg.GetDepth()))
	if err != nil {
		return nil, recallError(err)
	}
	out := make([]*rafikiv1.MemoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, memoryRow(r))
	}
	return connect.NewResponse(&rafikiv1.MemoryTreeResponse{Memories: out}), nil
}

func (s *Server) PutMemory(
	ctx context.Context, req *connect.Request[rafikiv1.PutMemoryRequest],
) (*connect.Response[rafikiv1.PutMemoryResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	meta := req.Msg.GetMetaJson()
	if meta == "" {
		meta = "{}"
	} else if !json.Valid([]byte(meta)) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("meta_json is not valid JSON"))
	}
	mem, err := m.PutMemory(ctx, recall.Memory{
		Path: req.Msg.GetPath(),
		Name: req.Msg.GetName(),
		Body: req.Msg.GetBody(),
		Meta: json.RawMessage(meta),
	})
	if err != nil {
		return nil, recallError(err)
	}
	return connect.NewResponse(&rafikiv1.PutMemoryResponse{Memory: memoryRow(mem)}), nil
}

func (s *Server) DeleteMemory(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteMemoryRequest],
) (*connect.Response[rafikiv1.DeleteMemoryResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	if err := m.DeleteMemory(ctx, req.Msg.GetPath(), req.Msg.GetName()); err != nil {
		return nil, recallError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteMemoryResponse{}), nil
}

func (s *Server) RecallBackfill(
	ctx context.Context, req *connect.Request[rafikiv1.RecallBackfillRequest],
) (*connect.Response[rafikiv1.RecallBackfillResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	if err := m.Backfill(ctx, time.Unix(req.Msg.GetSinceUnix(), 0).UTC(),
		req.Msg.GetMaxCostUsd()); err != nil {
		return nil, recallError(err)
	}
	return connect.NewResponse(&rafikiv1.RecallBackfillResponse{}), nil
}

func (s *Server) RecallStatus(
	ctx context.Context, req *connect.Request[rafikiv1.RecallStatusRequest],
) (*connect.Response[rafikiv1.RecallStatusResponse], error) {
	m, err := s.recallManager()
	if err != nil {
		return nil, err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return nil, recallError(err)
	}
	resp := &rafikiv1.RecallStatusResponse{
		Conversations:     st.Conversations,
		Windows:           st.Windows,
		WindowsUnembedded: st.WindowsUnembedded,
		Summaries:         st.Summaries,
		SummariesPending:  st.SummariesPending,
		Memories:          st.Memories,
		SummaryCostUsd:    st.SummaryCostUSD,
		BackfillBudgetUsd: st.BackfillBudgetUSD,
		BackfillSpentUsd:  st.BackfillSpentUSD,
		EmbeddingModel:    st.EmbeddingModel,
		SummaryModel:      st.SummaryModel,
	}
	if st.BackfillSince != nil {
		resp.BackfillSince = rfc3339(*st.BackfillSince)
	}
	return connect.NewResponse(resp), nil
}

// recallHit converts one fused hit for the wire. When is RFC3339 UTC.
func recallHit(h recall.Hit) *rafikiv1.RecallHit {
	return &rafikiv1.RecallHit{
		Id:               h.ID,
		Source:           string(h.Source),
		Snippet:          h.Snippet,
		When:             rfc3339(h.When),
		ConversationId:   h.ConversationID,
		ConversationName: h.ConversationName,
		Repo:             h.Repo,
		Kind:             h.Kind,
		OrdinalFrom:      int32(h.OrdinalFrom),
		OrdinalTo:        int32(h.OrdinalTo),
		Path:             h.Path,
		Name:             h.Name,
		Title:            h.Title,
		Score:            h.Score,
	}
}

// memoryRow converts one memory for the wire. Meta is stored as JSON and is
// "{}" when absent; the fallback covers a manager that hands back nil.
func memoryRow(m recall.Memory) *rafikiv1.MemoryRow {
	meta := m.Meta
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	return &rafikiv1.MemoryRow{
		Id:        m.ID,
		Path:      m.Path,
		Name:      m.Name,
		Body:      m.Body,
		MetaJson:  string(meta),
		CreatedAt: rfc3339(m.CreatedAt),
		UpdatedAt: rfc3339(m.UpdatedAt),
	}
}
