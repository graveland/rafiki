// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
)

// TestListChildrenSeedsCostFromTheCatalogPricedCoster pins the attach/startup
// cost seed end to end: a controller fresh from NewController, SetCatalog
// called once as main.go does, a captured turn, and the child summary carries
// a REAL priced number instead of a non-nil zero.
//
// This is a regression test for the WIRING, not for insights —
// CostsByConversation itself is covered in pkg/insights. The bug it guards:
// NewController built the coster as insights.New(pool) — pricer-less — and
// nothing ever attached the catalog's Pricing (only c.insights got it, in
// SetCatalog). An unpriced CostsByConversation short-circuits to no rows
// (pkg/insights/subtree.go), so every child seeded 0, the cockpit adopted the
// non-nil zero, and an attached previous session showed no cost even though
// every turn was captured.
func TestListChildrenSeedsCostFromTheCatalogPricedCoster(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Unique per run: openTestPool shares the RAFIKI_TEST_DSN database, and
	// conversation.external_ref is globally unique, so a fixed "c_proxy" would
	// collide with a leftover row from a previous run.
	uniq := fmt.Sprintf("_%d", time.Now().UnixNano())
	proxyChild := "c_proxy" + uniq

	// A fundi child's conversation is reachable by its SessionID (the
	// conversation UUID); a proxy child's by external_ref (X-Rafiki-Session:
	// <childID>). Cover both routes in one list, exactly as costsFor does.
	convID := insertConvForCost(t, pool, "") // no external_ref: fundi route
	proxyConv := insertConvForCost(t, pool, proxyChild)
	t.Cleanup(func() {
		// Clean the rows this test inserted so the shared test DB does not
		// accumulate them (and the unique external_ref does not collide next
		// run). Turns first: conversation_turn FKs to conversation.
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation_turn WHERE conversation_id = ANY($1::uuid[])`,
			[]string{convID, proxyConv})
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`,
			[]string{convID, proxyConv})
	})

	// $1 per 1M input + $2 per 1M output → 1_000_000 in + 500_000 out = $2.00.
	insertTurnForCost(t, pool, convID, "test/model", 1_000_000, 500_000)
	insertTurnForCost(t, pool, proxyConv, "test/model", 2_000_000, 0)

	st := childstore.New()
	dir := t.TempDir()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, pool, nil, ctx, nil, nil, nil)
	ctrl.SetCatalog(seedPricedCatalog(t, "test/model"))

	st.Insert(&childstore.Session{
		ChildID:   "c_fundi",
		SessionID: convID,
		Status:    protocol.StatusExited,
	})
	st.Insert(&childstore.Session{ChildID: proxyChild, Status: protocol.StatusExited})
	// A third child with NO turns: the query ran and found nothing, which is a
	// truthful zero and must stay distinct from "not known" (nil).
	st.Insert(&childstore.Session{
		ChildID:   "c_idle",
		SessionID: "00000000-0000-0000-0000-0000000000aa",
		Status:    protocol.StatusExited,
	})

	sums := ctrl.ListChildren(nil)
	byID := map[string]protocol.ChildSummary{}
	for _, s := range sums {
		byID[s.ChildID] = s
	}
	if len(byID) != 3 {
		t.Fatalf("ListChildren returned %d rows, want 3", len(byID))
	}

	if got := byID["c_fundi"].CostUSD; got == nil {
		t.Error("c_fundi: CostUSD is nil — the catalog-priced coster never seeded it")
	} else if math.Abs(*got-2.0) > 1e-9 {
		t.Errorf("c_fundi cost = %v, want 2.0 (priced from the catalog)", *got)
	}
	if got := byID[proxyChild].CostUSD; got == nil {
		t.Error("c_proxy: CostUSD is nil — the external_ref route seeded nothing")
	} else if math.Abs(*got-2.0) > 1e-9 {
		t.Errorf("c_proxy cost = %v, want 2.0 (matched by external_ref)", *got)
	}
	if got := byID["c_idle"].CostUSD; got != nil && *got != 0 {
		t.Errorf("c_idle cost = %v, want 0 (query ran, found no turns)", *got)
	}
}

// seedPricedCatalog builds a catalog whose Pricing resolves model, so the
// coster SetCatalog builds from it actually prices turns.
func seedPricedCatalog(t *testing.T, model string) *routing.ModelCatalog {
	t.Helper()
	c := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]routing.CatalogEntry{{
		ID: model,
		Pricing: &routing.ModelPricing{
			PromptUSD:     1e-6, // $1 per 1M input
			CompletionUSD: 2e-6, // $2 per 1M output
		},
	}})
	return c
}

func insertConvForCost(t *testing.T, pool *pgxpool.Pool, extRef string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (persona, model, origin_entrypoint, driven_by, external_ref)
		 VALUES ('test', 'test/model', 'test', 'server', NULLIF($1, '')) RETURNING id::text`,
		extRef).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

func insertTurnForCost(t *testing.T, pool *pgxpool.Pool, convID, model string, inTok, outTok int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.conversation_turn
		   (conversation_id, ordinal, status, model, request, response, stop_reason,
		    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		    upstream, latency_ms, source, created_at)
		 VALUES ($1, 0, 'complete', $2, '{}'::jsonb, NULL, 'end_turn',
		         $3, $4, 0, 0, 'openrouter', 100, 'test', now())`,
		convID, model, inTok, outTok)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
}
