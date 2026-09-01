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
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if f.gotProvider != "anthropic" || f.gotKind != "claude" {
		t.Errorf("got (%q,%q), want (anthropic,claude)", f.gotProvider, f.gotKind)
	}
}

// The whole point of the pointer fields: a model the catalog does not know
// must arrive with them ABSENT, not zeroed.
func TestListModelsUnknownCatalogFieldsStayAbsent(t *testing.T) {
	f := &fakeModelLister{rows: []connectapi.ModelRow{{
		ID: "ollama/llama3", Provider: "ollama", Model: "llama3", Source: "local",
	}}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	got := resp.Msg.GetModels()[0]
	if got.ContextWindow != nil {
		t.Errorf("ContextWindow = %v, want nil", got.ContextWindow)
	}
	if got.PromptUsd != nil {
		t.Errorf("PromptUsd = %v, want nil", got.PromptUsd)
	}
	if len(got.GetInputModalities()) != 0 {
		t.Errorf("InputModalities = %v, want empty", got.GetInputModalities())
	}
}

func TestListModelsCarriesKnownCatalogFields(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	got := resp.Msg.GetModels()[0]
	if got.GetContextWindow() != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", got.GetContextWindow())
	}
	if got.GetPromptUsd() != 0.000005 {
		t.Errorf("PromptUsd = %v, want 0.000005", got.GetPromptUsd())
	}
	if len(got.GetInputModalities()) != 2 {
		t.Errorf("InputModalities = %v, want 2 entries", got.GetInputModalities())
	}
}

// A zero price is a REAL price and must survive as present-and-zero.
func TestListModelsZeroPriceIsPresent(t *testing.T) {
	f := &fakeModelLister{rows: []connectapi.ModelRow{{
		ID: "x/free", PromptUSD: f64p(0),
	}}}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	resp, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if resp.Msg.GetModels()[0].PromptUsd == nil {
		t.Fatal("PromptUsd = nil for an explicitly free model; zero must stay present")
	}
}

func TestListModelsWithoutListerFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestListModelsErrorBecomesInternal(t *testing.T) {
	f := &fakeModelLister{err: errors.New("catalog down")}
	s := connectapi.NewServer(nil)
	s.SetModelLister(f)

	_, err := s.ListModels(context.Background(),
		connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}
