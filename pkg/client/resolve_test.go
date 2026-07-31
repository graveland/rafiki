package client_test

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// fakeLister is an in-memory Lister used by resolver tests. No real socket
// or wire format is involved, so these tests run very fast.
type fakeLister struct {
	children []protocol.ChildSummary
}

func (f *fakeLister) List(_ context.Context, _ protocol.ListFilter) ([]protocol.ChildSummary, error) {
	return f.children, nil
}

// TestResolve_ExactChildID exercises the fast path: input that starts with
// "c_" is returned as-is without calling List at all.
func TestResolve_ExactChildID(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk"},
		{ChildID: "c_01HX02", Name: "other"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "c_01HX01")
	if err != nil {
		t.Fatal(err)
	}
	if got != "c_01HX01" {
		t.Fatalf("got %q, want %q", got, "c_01HX01")
	}
}

// TestResolve_ExactName checks that an exact name match returns the right childId.
func TestResolve_ExactName(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "afk-impl")
	if err != nil {
		t.Fatal(err)
	}
	if got != "c_01HX01" {
		t.Fatalf("got %q, want %q", got, "c_01HX01")
	}
}

// TestResolve_PrefixMatch checks that a unique prefix resolves to the single
// matching child without requiring the full name.
func TestResolve_PrefixMatch(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl-2026"},
		{ChildID: "c_01HX02", Name: "other"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "afk")
	if err != nil {
		t.Fatal(err)
	}
	if got != "c_01HX01" {
		t.Fatalf("got %q, want %q", got, "c_01HX01")
	}
}

// TestResolve_AmbiguousPrefix_Errors checks that a prefix matching multiple
// children produces an error that names the candidates.
func TestResolve_AmbiguousPrefix_Errors(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl"},
		{ChildID: "c_01HX02", Name: "afk-test"},
	}}
	_, err := client.ResolveWith(context.Background(), f, "afk")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error doesn't mention ambiguity: %v", err)
	}
}

// TestResolve_NoMatch_Errors checks that an identifier with no match at all
// returns a non-nil error.
func TestResolve_NoMatch_Errors(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk"},
	}}
	_, err := client.ResolveWith(context.Background(), f, "nope")
	if err == nil {
		t.Fatal("expected no-match error, got nil")
	}
}
