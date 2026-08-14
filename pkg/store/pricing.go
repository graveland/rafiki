// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/pricing"
)

// ModelPrice is a per-token USD price for one model. Re-exported from
// pkg/pricing (the DB-free home of the price types) so this package's callers
// and the catalog share one definition; the alias keeps store.ModelPrice valid
// without re-declaring the struct.
type ModelPrice = pricing.ModelPrice

// ModelInfo is everything price syncing needs about one model, resolved in a
// single catalog lookup. See pkg/pricing.
type ModelInfo = pricing.ModelInfo

// PriceSource is the slice of a model catalog that price syncing needs. See
// pkg/pricing.
type PriceSource = pricing.PriceSource

// SyncModelPricing refreshes conversations.model_pricing from src. It writes
// two sets of keys so a lookup succeeds however the turn spelled the model:
// every catalog id, and every distinct model string actually observed on a
// turn.
//
// A model observed on a turn that src cannot resolve still gets a row, with
// or_id and every price NULL. That is deliberate: the views translate a
// missing row and a NULL price identically into unpriced=true, and a visible
// gap in a spend dashboard beats a silent $0.
//
// Prices are current list prices, not the price at the time of the turn — cost
// figures re-price if the source changes a rate. Returns the number of rows
// upserted.
func SyncModelPricing(ctx context.Context, pool *pgxpool.Pool, src PriceSource) (int, error) {
	if src == nil {
		return 0, fmt.Errorf("sync model pricing: nil price source")
	}
	src.Warm()

	observed, err := distinctTurnModels(ctx, pool)
	if err != nil {
		return 0, err
	}

	// Catalog ids first, observed strings second; a string that is itself a
	// catalog id simply upserts twice to the same row. Built fresh rather than
	// appended onto AllIDs()' return: that slice belongs to the source, and
	// appending would write into whatever spare capacity it happens to have.
	ids := src.AllIDs()
	keys := make([]string, 0, len(ids)+len(observed))
	keys = append(keys, ids...)
	keys = append(keys, observed...)

	batch := &pgx.Batch{}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		// Every price is a pointer so an unresolved or unpriced model writes
		// NULL rather than a zero that would read as free.
		var orID *string
		var prompt, completion, cacheRead, cacheWrite *float64
		if info, ok := src.Lookup(key); ok {
			orID = &info.ORID
			if info.Priced {
				prompt, completion = &info.Price.PromptUSD, &info.Price.CompletionUSD
				cacheRead, cacheWrite = info.Price.CacheReadUSD, info.Price.CacheWriteUSD
			}
		}

		batch.Queue(`
			INSERT INTO conversations.model_pricing
				(model_id, or_id, prompt_usd, completion_usd, cache_read_usd,
				 cache_write_usd, fetched_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (model_id) DO UPDATE SET
				or_id           = EXCLUDED.or_id,
				prompt_usd      = EXCLUDED.prompt_usd,
				completion_usd  = EXCLUDED.completion_usd,
				cache_read_usd  = EXCLUDED.cache_read_usd,
				cache_write_usd = EXCLUDED.cache_write_usd,
				fetched_at      = now()`,
			key, orID, prompt, completion, cacheRead, cacheWrite)
	}

	if batch.Len() == 0 {
		return 0, nil
	}
	if err := pool.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("sync model pricing: upsert: %w", err)
	}
	return batch.Len(), nil
}

// distinctTurnModels returns the model strings worth pricing: those seen on a
// turn recently, plus every model already in model_pricing.
//
// The window is not an optimization detail — conversation_turn is a columnstore
// hypertable segmented by conversation_id with no index on model, so an
// unbounded DISTINCT decompresses every chunk ever written, growing without
// bound while the set of distinct model strings does not.
//
// The union is what makes the window safe: without it, a model that stopped
// being used would drop out of the sync, stop having its price refreshed, and
// then silently drift away from the real rate while still pricing old turns.
func distinctTurnModels(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT model FROM conversations.conversation_turn
		 WHERE model IS NOT NULL AND created_at > now() - interval '30 days'
		UNION
		SELECT model_id FROM conversations.model_pricing`)
	if err != nil {
		return nil, fmt.Errorf("sync model pricing: distinct turn models: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("sync model pricing: scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModelPricingCount reports how many pricing rows exist. The scheduler uses it
// to decide whether a cold start needs an immediate sync.
func ModelPricingCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_pricing`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count model pricing: %w", err)
	}
	return n, nil
}
