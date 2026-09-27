// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeModelLister struct {
	gotProvider string
	gotKind     string
	rows        []connectapi.ModelRow
	err         error
}

func (f *fakeModelLister) ListModels(_ context.Context, provider, kind string) ([]connectapi.ModelRow, error) {
	f.gotProvider, f.gotKind = provider, kind
	return f.rows, f.err
}

func intp(v int) *int         { return &v }
func f64p(v float64) *float64 { return &v }

func TestListModelsPassesFiltersThrough(t *testing.T) {
	f := &fakeModelLister{}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	_, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{Provider: "anthropic", Kind: "claude"}))
	assert.NewAborting(t).NoError(err, "ListModels")
	if f.gotProvider != "anthropic" || f.gotKind != "claude" {
		t.Errorf("got (%q,%q), want (anthropic,claude)", f.gotProvider, f.gotKind)
	}
}

// The whole point of the pointer fields: a model the catalog does not know
// must arrive with them ABSENT, not zeroed.
func TestListModelsUnknownCatalogFieldsStayAbsent(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeModelLister{rows: []connectapi.ModelRow{{
		ID: "ollama/llama3", Provider: "ollama", Model: "llama3", Source: "local",
	}}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Require().NoError(err, "ListModels")
	got := resp.Msg.GetModels()[0]
	c.Nil(got.ContextWindow, "ContextWindow")
	c.Nil(got.PromptUsd, "PromptUsd")
	c.Empty(got.GetInputModalities(), "InputModalities")
}

func TestListModelsCarriesKnownCatalogFields(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeModelLister{rows: []connectapi.ModelRow{{
		ID: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o",
		Name: "GPT-4o", Source: "openrouter",
		ContextWindow: intp(128000), MaxCompletionTokens: intp(16384),
		PromptUSD: f64p(0.000005), CompletionUSD: f64p(0.000015),
		InputModalities: []string{"text", "image"},
	}}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Require().NoError(err, "ListModels")
	got := resp.Msg.GetModels()[0]
	c.Eq(128000, got.GetContextWindow(), "ContextWindow")
	c.Eq(0.000005, got.GetPromptUsd(), "PromptUsd")
	c.Len(got.GetInputModalities(), 2, "InputModalities")
}

// A zero price is a REAL price and must survive as present-and-zero.
func TestListModelsZeroPriceIsPresent(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeModelLister{rows: []connectapi.ModelRow{{
		ID: "x/free", PromptUSD: f64p(0),
	}}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.NoError(err, "ListModels")
	c.NotNil(resp.Msg.GetModels()[0].PromptUsd, "PromptUsd = nil for an explicitly free model; zero must stay present")
}

func TestListModelsWithoutListerFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestListModelsErrorBecomesInternal(t *testing.T) {
	f := &fakeModelLister{err: errors.New("catalog down")}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	_, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInternal, connect.CodeOf(err), "code")
}

// Tool support, listing date and expiry ride the wire, and absence survives.
func TestListModelsCarriesToolSupportAgeAndExpiry(t *testing.T) {
	c := assert.NewCollecting(t)
	created := int64(1750000000)
	f := &fakeModelLister{rows: []connectapi.ModelRow{
		{
			ID: "a/agentic", Created: &created, ExpiresAt: "2026-09-08",
			SupportedParameters: []string{"tools", "temperature"},
		},
		{ID: "ollama/llama3"}, // no catalog entry at all
	}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Require().NoError(err, "ListModels")
	got := resp.Msg.GetModels()

	c.Eq(created, got[0].GetCreated(), "Created")
	c.Eq("2026-09-08", got[0].GetExpiresAt(), "ExpiresAt")
	c.Len(got[0].GetSupportedParameters(), 2, "SupportedParameters")

	// A model with no catalog entry must arrive UNKNOWN on all three, never as
	// "supports nothing" or "created at the epoch".
	c.Nil(got[1].Created, "Created")
	c.Empty(got[1].GetSupportedParameters(), "SupportedParameters")
	c.Eq("", got[1].GetExpiresAt(), "ExpiresAt")
}

func TestListModelsCarriesCutoffAndAgenticScore(t *testing.T) {
	c := assert.NewCollecting(t)
	score := 59.2
	f := &fakeModelLister{rows: []connectapi.ModelRow{
		{ID: "a/scored", KnowledgeCutoff: "2026-02-16", AgenticIndex: &score},
		{ID: "b/unscored"},
	}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Require().NoError(err, "ListModels")
	got := resp.Msg.GetModels()

	c.Eq("2026-02-16", got[0].GetKnowledgeCutoff(), "KnowledgeCutoff =")
	c.Eq(score, got[0].GetAgenticIndex(), "AgenticIndex")
	// Absence must survive the wire: a nil score must not arrive as 0, which
	// would sort as the worst model rather than as no answer.
	c.Nil(got[1].AgenticIndex, "AgenticIndex")
	c.Eq("", got[1].GetKnowledgeCutoff(), "KnowledgeCutoff")
}
