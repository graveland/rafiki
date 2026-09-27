// SPDX-License-Identifier: Apache-2.0

package main

// Unit tests for the Connect recall adapter: the identity-derived scope and
// memory owner (a request can never widen either) and the backfill gates
// (admin-only, positive budget required — the review-3.1 addendum's zero
// asymmetry). The handlers themselves are pinned in pkg/connectapi.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// fakeConnectRecallStore answers the calls the connect adapter makes; anything
// else panics through the embedded nil interface, keeping the fake small. The
// same fail-closed contract recalldb implements: an invalid scope admits
// nothing, before any other work.
type fakeConnectRecallStore struct {
	recall.Store

	scope  recall.Scope
	owner  string
	hits   []recall.Hit
	states map[string]string
}

func (f *fakeConnectRecallStore) SearchBM25(_ context.Context, q recall.SearchQuery, _ recall.Source) ([]recall.Hit, error) {
	f.scope, f.owner = q.Scope, q.MemoryOwner
	if !q.Scope.Valid() {
		return nil, recall.ErrInvalidScope
	}
	return f.hits, nil
}

func (f *fakeConnectRecallStore) SetState(_ context.Context, key, value string) error {
	if f.states == nil {
		f.states = map[string]string{}
	}
	f.states[key] = value
	return nil
}

func (f *fakeConnectRecallStore) Status(_ context.Context) (recall.Status, error) {
	return recall.Status{}, nil
}

// connectRecallWith wires a controller whose recall runtime is backed by st —
// the shape startRecall leaves behind, with a real (never Run) indexer so
// Backfill's Nudge has somewhere to go.
func connectRecallWith(st recall.Store) connectRecall {
	return connectRecall{c: &Controller{recall: &recallRuntime{
		st:      st,
		indexer: recall.NewIndexer(recall.IndexerOptions{Store: st}),
	}}}
}

// TestConnectRecallOverridesScopeFromIdentity pins the adapter's core rule:
// whatever SearchQuery arrives, the store sees the identity-derived scope and
// memory owner — admin → all rows, a user → their own, an anonymous unix-socket
// caller → the zero Scope the store refuses (PermissionDenied), and the memory
// owner is always the caller's own id.
func TestConnectRecallOverridesScopeFromIdentity(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeConnectRecallStore{}
	s := &connectapi.Server{}
	s.SetRecallManager(connectRecallWith(fs))

	// A poisoned query cannot widen the caller's reach: the adapter overwrites
	// Scope and MemoryOwner before the store sees them.
	poisoned := recall.SearchQuery{
		Scope:       recall.Scope{All: true},
		MemoryOwner: "someone-else",
		Text:        "needle",
	}
	admin := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u-admin", Username: "brent", Via: server.ProvenanceUser, IsAdmin: true})
	hits, err := connectRecallWith(fs).Recall(admin, poisoned, 10)
	c.NoError(err, "admin recall")
	if fs.scope != (recall.Scope{All: true}) || fs.owner != "u-admin" {
		t.Fatalf("admin store saw scope %+v owner %q, want All/u-admin", fs.scope, fs.owner)
	}
	c.Empty(hits, "fake returned %d hits, want 0", len(hits))

	// Through the handler: the wire carries no scope at all, and the identity
	// still lands on the store.
	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u-bob", Via: server.ProvenanceUser})
	if _, err := s.Recall(user, connect.NewRequest(&rafikiv1.RecallRequest{Query: "x"})); err != nil {
		t.Fatalf("user recall: %v", err)
	}
	if fs.scope != (recall.Scope{OwnerUserID: "u-bob"}) || fs.owner != "u-bob" {
		t.Fatalf("user store saw scope %+v owner %q, want u-bob's own", fs.scope, fs.owner)
	}

	// Anonymous: the zero Scope reaches the store and is refused there.
	_, err = s.Recall(context.Background(), connect.NewRequest(&rafikiv1.RecallRequest{Query: "x"}))
	c.False(!errors.Is(err, recall.ErrInvalidScope) || connect.CodeOf(err) != connect.CodePermissionDenied, "anonymous recall err = %v, want PermissionDenied wrapping ErrInvalidScope", err)
	c.Eq((recall.Scope{}), fs.scope, "anonymous store saw scope")
}

// TestConnectRecallBackfillAdminOnly pins the backfill gates: a non-admin is
// refused (PermissionDenied) without touching state, a non-positive budget is
// refused (InvalidArgument) — a wire RecallBackfillRequest{} must never arm an
// all-history, budgetless backfill — and an admin with a positive budget arms
// exactly the three state keys, with spent reset to "0".
func TestConnectRecallBackfillAdminOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &connectapi.Server{}
	fs := &fakeConnectRecallStore{}
	s.SetRecallManager(connectRecallWith(fs))

	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u-bob", Via: server.ProvenanceUser})
	_, err := s.RecallBackfill(user, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
		SinceUnix: 1700000000, MaxCostUsd: 5,
	}))
	c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "non-admin backfill: got code")
	c.Require().Empty(fs.states, "refused backfill wrote state")

	admin := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u-admin", Username: "brent", Via: server.ProvenanceUser, IsAdmin: true})
	for _, cost := range []float64{0, -1} {
		_, err := s.RecallBackfill(admin, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
			SinceUnix: 1700000000, MaxCostUsd: cost,
		}))
		c.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "backfill with max_cost_usd %v: got code %v, want InvalidArgument", cost, connect.CodeOf(err))
		c.Require().ErrorIs(err, connectapi.ErrNoBackfillBudget, "budget refusal does not wrap ErrNoBackfillBudget")
	}
	// The zero-value request itself: both fields zero.
	if _, err := s.RecallBackfill(admin, connect.NewRequest(&rafikiv1.RecallBackfillRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("zero-value RecallBackfillRequest: got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	c.Require().Empty(fs.states, "refused backfill wrote state")

	_, err = s.RecallBackfill(admin, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
		SinceUnix: 1700000000, MaxCostUsd: 2.5,
	}))
	c.Require().NoError(err, "admin backfill")
	want := map[string]string{
		"backfill_since":      "2023-11-14T22:13:20Z", // RFC3339 of unix 1700000000
		"backfill_budget_usd": "2.5",
		"backfill_spent_usd":  "0",
	}
	for k, v := range want {
		c.Eq(v, fs.states[k], "state[%s] = %q, want", k, fs.states[k])
	}
	c.Len(fs.states, len(want), "backfill wrote %d keys (%v), want exactly %v", len(fs.states), fs.states, want)
}

// TestConnectRecallStatusCarriesModels pins the two models the store does not
// know: the embedder's (empty when BM25-only) and the configured summarizer's.
func TestConnectRecallStatusCarriesModels(t *testing.T) {
	ck := assert.NewAborting(t)
	rt := &recallRuntime{
		st:           &fakeConnectRecallStore{},
		summaryModel: "sum-model",
	}
	m := connectRecall{c: &Controller{recall: rt}}
	st, err := m.Status(context.Background())
	ck.NoError(err, "status without embedder")
	ck.False(st.EmbeddingModel != "" || st.SummaryModel != "sum-model", "status = %+v, want empty embedding model and sum-model", st)
}
