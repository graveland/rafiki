// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// errRecallUnwired is the adapter's refusal when the daemon's recall runtime
// is absent. Unreachable through the Connect wiring — the manager is set only
// when ctrl.recall is non-nil — and never by request action; recallError maps
// it to CodeInternal, the honest code for a daemon-side gap.
var errRecallUnwired = errors.New("recall: backend not wired")

// connectRecall adapts *Controller to connectapi.RecallManager, modelled on
// connectPresets: the caller's identity comes from the request CONTEXT
// (server.IdentityFromContext), never from a request field. An anonymous
// unix-socket caller resolves to the zero identity, which the scope rule
// degrades to the zero Scope (the store refuses it with ErrInvalidScope
// before any SQL) and the memory rule to an empty owner (ErrNoOwner) — the
// same fail-closed shape the in-process recall tools give that caller.
type connectRecall struct{ c *Controller }

var _ connectapi.RecallManager = connectRecall{}

// recallIdentity resolves the caller exactly as connectPresets does, keeping
// IsAdmin — which spawnOwner drops and which the scope rule (admin → all) and
// the backfill gate need. A child-attributed identity carries its owner's
// UserID, so a child reads and writes through its owner, the same attribution
// path the spawn plane bills through.
func recallIdentity(ctx context.Context) users.Identity {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return users.Identity{}
	}
	return users.Identity{UserID: id.UserID, Username: id.Username, IsAdmin: id.IsAdmin}
}

// recallScopeFor is newRecallBinding's rule: admin → everything, a named user
// → their own rows, anyone else → the zero Scope that admits nothing.
func recallScopeFor(owner users.Identity) recall.Scope {
	switch {
	case owner.IsAdmin:
		return recall.Scope{All: true}
	case owner.UserID != "":
		return recall.Scope{OwnerUserID: owner.UserID}
	default:
		return recall.Scope{}
	}
}

// runtime returns the daemon's wired recall subsystem.
func (m connectRecall) runtime() (*recallRuntime, error) {
	if m.c.recall == nil {
		return nil, errRecallUnwired
	}
	return m.c.recall, nil
}

// Recall runs the caller's search. The adapter OVERWRITES q.Scope and
// q.MemoryOwner from the resolved identity — the wire carries no scope, and
// if it ever did, trusting it would let any caller widen their reach. The
// memory owner is always the caller's own id, admin included.
func (m connectRecall) Recall(ctx context.Context, q recall.SearchQuery, limit int) ([]recall.Hit, error) {
	rt, err := m.runtime()
	if err != nil {
		return nil, err
	}
	owner := recallIdentity(ctx)
	q.Scope = recallScopeFor(owner)
	q.MemoryOwner = owner.UserID
	return recall.Search(ctx, rt.st, rt.emb, q, limit)
}

// RecallContext expands one hit id through the same binding the in-process
// recall tools use, so the CLI and the tools render a hit identically.
func (m connectRecall) RecallContext(ctx context.Context, id string, before, after, maxChars int) (string, error) {
	if _, err := m.runtime(); err != nil {
		return "", err
	}
	b := newRecallBinding(m.c, recallIdentity(ctx))
	if b == nil {
		return "", errRecallUnwired
	}
	return b.Context(ctx, id, before, after, maxChars)
}

// GetMemory returns one of the CALLER's memories — the owner is resolved from
// ctx, never a request field, so an admin's get reaches only their own rows.
func (m connectRecall) GetMemory(ctx context.Context, path, name string) (recall.Memory, error) {
	rt, err := m.runtime()
	if err != nil {
		return recall.Memory{}, err
	}
	return rt.st.GetMemory(ctx, recallIdentity(ctx).UserID, path, name)
}

// MemoryTree lists the caller's memories under a path.
func (m connectRecall) MemoryTree(ctx context.Context, path string, depth int) ([]recall.Memory, error) {
	rt, err := m.runtime()
	if err != nil {
		return nil, err
	}
	return rt.st.MemoryTree(ctx, recallIdentity(ctx).UserID, path, depth)
}

// PutMemory saves a memory owned by the caller.
func (m connectRecall) PutMemory(ctx context.Context, mem recall.Memory) (recall.Memory, error) {
	rt, err := m.runtime()
	if err != nil {
		return recall.Memory{}, err
	}
	return rt.st.PutMemory(ctx, recallIdentity(ctx).UserID, mem)
}

// DeleteMemory tombstones the caller's memory at (path, name).
func (m connectRecall) DeleteMemory(ctx context.Context, path, name string) error {
	rt, err := m.runtime()
	if err != nil {
		return err
	}
	return rt.st.DeleteMemory(ctx, recallIdentity(ctx).UserID, path, name)
}

// Backfill arms the summary backfill. Admin-only: any other caller reads as
// recall.ErrInvalidScope, which recallError maps to PermissionDenied. A
// positive budget is REQUIRED — recallError maps connectapi.ErrNoBackfillBudget
// to InvalidArgument — so a wire RecallBackfillRequest{} (both fields zero)
// can never arm an all-history, budgetless backfill; since epoch is the
// deliberate "from the beginning" spelling of an operator who typed a
// timestamp. Arming is three state writes — backfill_since (RFC3339),
// backfill_budget_usd, and backfill_spent_usd reset to "0" so a previous
// backfill's spend does not count against the new budget — followed by one
// indexer nudge, which is what turns the new lower bound into work.
func (m connectRecall) Backfill(ctx context.Context, since time.Time, maxCostUSD float64) error {
	rt, err := m.runtime()
	if err != nil {
		return err
	}
	if !recallIdentity(ctx).IsAdmin {
		return recall.ErrInvalidScope
	}
	if maxCostUSD <= 0 {
		return fmt.Errorf("%w: got %v", connectapi.ErrNoBackfillBudget, maxCostUSD)
	}
	if err := rt.st.SetState(ctx, "backfill_since", since.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := rt.st.SetState(ctx, "backfill_budget_usd", strconv.FormatFloat(maxCostUSD, 'f', -1, 64)); err != nil {
		return err
	}
	if err := rt.st.SetState(ctx, "backfill_spent_usd", "0"); err != nil {
		return err
	}
	rt.indexer.Nudge()
	return nil
}

// Status reads the store's counts plus the two models the store does not
// know: the embedder's model (empty when search is BM25-only) and the
// configured summarizer model (empty when summaries are off).
func (m connectRecall) Status(ctx context.Context) (connectapi.RecallStatus, error) {
	rt, err := m.runtime()
	if err != nil {
		return connectapi.RecallStatus{}, err
	}
	st, err := rt.st.Status(ctx)
	if err != nil {
		return connectapi.RecallStatus{}, err
	}
	out := connectapi.RecallStatus{Status: st, SummaryModel: rt.summaryModel}
	if rt.emb != nil {
		out.EmbeddingModel = rt.emb.Model()
	}
	return out, nil
}
