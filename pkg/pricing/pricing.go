// SPDX-License-Identifier: Apache-2.0

// Package pricing holds the pure-data price types shared between the model
// catalog and the price-syncing writer. They live here, in a package with no
// database dependency, so the catalog (pkg/routing) can import them without
// linking pgx — pkg/store re-exports them as aliases for its own callers.
package pricing

// ModelPrice is a per-token USD price for one model. The cache fields are
// pointers because absent and zero are different facts: OpenRouter omits them
// for models without prompt caching, and writing 0 for "not priced" makes
// v_turn.cache_saved_usd compute the cache tokens at the full prompt price —
// a saving overstated by the entire cache discount.
//
// There is deliberately no 1h cache-write price: conversation_turn records no
// cache TTL, so nothing downstream can tell a 1h write from a 5m one.
type ModelPrice struct {
	PromptUSD     float64
	CompletionUSD float64
	CacheReadUSD  *float64 // nil = the source does not price cache reads
	CacheWriteUSD *float64 // nil = the source does not price cache writes
}

// ModelInfo is everything price syncing needs about one model, resolved in a
// single catalog lookup. Priced is false when the entry carries no parseable
// base price, in which case Price is zero and must not be written.
type ModelInfo struct {
	ORID   string
	Price  ModelPrice
	Priced bool
}

// PriceSource is the slice of a model catalog that price syncing needs. It is
// declared here, in the DB-free package the catalog already imports, so that
// the catalog can satisfy it without depending on the package that owns the
// conversations schema (pkg/store, which links pgx) — routing depends on
// store today, and the reverse would be an import cycle.
//
// Lookup is deliberately a single call rather than separate id/price/context
// accessors: the catalog resolves a model by scanning its snapshot under a
// mutex, and this runs over every catalog id (~500) plus every observed model
// string against a catalog shared with the live proxy's request path.
type PriceSource interface {
	Warm()
	AllIDs() []string
	Lookup(model string) (ModelInfo, bool)
}
