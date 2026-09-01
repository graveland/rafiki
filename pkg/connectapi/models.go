// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ModelRow is one model the daemon can run, plus whatever the catalog knows
// about it.
//
// Every catalog field is a POINTER or a nil slice because absent and zero
// differ: a locally-served model has no catalog entry at all, and "unpriced"
// must stay distinguishable from "free". Same rule every Usage field follows.
// Prices are USD PER TOKEN, as OpenRouter reports them.
//
// This is a package-local domain type rather than a pkg/models type carrying a
// routing.ModelPricing: cmd/rafiki links pkg/models, and that edge would drag
// the routing stack into the thin client for nothing.
type ModelRow struct {
	ID       string
	Provider string
	Model    string
	Name     string
	Source   string

	ContextWindow       *int
	MaxCompletionTokens *int
	PromptUSD           *float64
	CompletionUSD       *float64
	CacheReadUSD        *float64
	CacheWriteUSD       *float64

	// InputModalities is nil when the daemon has no catalog entry for this id.
	// That means UNKNOWN, never "accepts nothing" — a text-only model reports
	// ["text"].
	InputModalities []string
}

// ModelLister is the narrow slice of the daemon's Controller needed to
// enumerate models. kind scopes which sources may answer; empty means the
// fundi default.
type ModelLister interface {
	ListModels(ctx context.Context, provider, kind string) ([]ModelRow, error)
}

// SetModelLister attaches the model source. Post-construction setter for the
// same reason as SetChildLister: the Controller is built after this Server.
func (s *Server) SetModelLister(l ModelLister) { s.modelLister.Store(&l) }

// ListModels enumerates the models the daemon can run.
func (s *Server) ListModels(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListModelsRequest],
) (*connect.Response[rafikiv1.ListModelsResponse], error) {
	p := s.modelLister.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("model lister not yet wired"))
	}
	rows, err := (*p).ListModels(ctx, req.Msg.GetProvider(), req.Msg.GetKind())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*rafikiv1.ModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoModel(r))
	}
	return connect.NewResponse(&rafikiv1.ListModelsResponse{Models: out}), nil
}

// toProtoModel maps one row onto the wire type, preserving absence on every
// optional field. A value copy is taken per field because &r.Field would alias
// the loop variable's storage.
func toProtoModel(r ModelRow) *rafikiv1.ModelRow {
	out := &rafikiv1.ModelRow{
		Id:              r.ID,
		Provider:        r.Provider,
		Model:           r.Model,
		Name:            r.Name,
		Source:          r.Source,
		InputModalities: r.InputModalities,
	}
	if r.ContextWindow != nil {
		v := int32(*r.ContextWindow)
		out.ContextWindow = &v
	}
	if r.MaxCompletionTokens != nil {
		v := int32(*r.MaxCompletionTokens)
		out.MaxCompletionTokens = &v
	}
	if r.PromptUSD != nil {
		v := *r.PromptUSD
		out.PromptUsd = &v
	}
	if r.CompletionUSD != nil {
		v := *r.CompletionUSD
		out.CompletionUsd = &v
	}
	if r.CacheReadUSD != nil {
		v := *r.CacheReadUSD
		out.CacheReadUsd = &v
	}
	if r.CacheWriteUSD != nil {
		v := *r.CacheWriteUSD
		out.CacheWriteUsd = &v
	}
	return out
}
