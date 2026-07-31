// SPDX-License-Identifier: Apache-2.0

// Package insights reads the captured conversations schema (conversation,
// conversation_turn, conversation_message) for analysis surfaces: search,
// transcript export, aggregate stats, and a sanitized read-only query executor.
// It is read-only — nothing here writes to the store.
package insights

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timescale/rafiki/routing"
)

// ErrNotFound is returned when a requested conversation does not exist. Callers
// (e.g. the gRPC handler) match it with errors.Is to map to a NotFound status.
var ErrNotFound = errors.New("insights: conversation not found")

// Pricer resolves a model id to its per-token list price. It is injected (the
// server passes ModelCatalog.Pricing) so insights carries no catalog/network
// concern. ok=false leaves the model unpriced.
type Pricer func(model string) (routing.ModelPricing, bool)

// Insights answers analysis queries over the conversations schema.
type Insights struct {
	pool   *pgxpool.Pool
	pricer Pricer
}

// New returns an Insights backed by pool. It is unpriced until WithPricer is set.
func New(pool *pgxpool.Pool) *Insights { return &Insights{pool: pool} }

// WithPricer sets the price resolver used to fill CostRow.CostUSD and returns
// the receiver for chaining. Safe to call with nil (leaves cost unpriced).
func (i *Insights) WithPricer(p Pricer) *Insights {
	i.pricer = p
	return i
}

// Path selects a capture path in filters. It maps to the immutable driven_by
// column: the proxy (client-driven) path vs. the in-process (server-driven) one.
type Path string

const (
	PathAny    Path = ""       // no path filter
	PathProxy  Path = "proxy"  // driven_by = 'client'
	PathDirect Path = "direct" // driven_by = 'server'
)

// validate rejects a non-empty Path that is neither proxy nor direct, so a
// typo'd filter errors loudly instead of silently returning unfiltered results.
func (p Path) validate() error {
	switch p {
	case PathAny, PathProxy, PathDirect:
		return nil
	default:
		return fmt.Errorf("invalid path %q: use %q or %q", string(p), PathProxy, PathDirect)
	}
}

// drivenBy maps a Path filter to the driven_by column value, or "" for no filter.
func (p Path) drivenBy() string {
	switch p {
	case PathProxy:
		return "client"
	case PathDirect:
		return "server"
	default:
		return ""
	}
}

// argList accumulates positional query arguments and hands back the $N
// placeholder for each, so a dynamic WHERE stays parameterized.
type argList struct{ args []any }

func (a *argList) next(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("$%d", len(a.args))
}
