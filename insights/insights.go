// Package insights reads the captured conversations schema (conversation,
// conversation_turn, conversation_message) for analysis surfaces: search,
// transcript export, aggregate stats, and a sanitized read-only query executor.
// It is read-only — nothing here writes to the store.
package insights

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Insights answers analysis queries over the conversations schema.
type Insights struct{ pool *pgxpool.Pool }

// New returns an Insights backed by pool.
func New(pool *pgxpool.Pool) *Insights { return &Insights{pool: pool} }

// Path selects a capture path in filters. It maps to the immutable driven_by
// column: the proxy (client-driven) path vs. the in-process (server-driven) one.
type Path string

const (
	PathAny    Path = ""       // no path filter
	PathProxy  Path = "proxy"  // driven_by = 'client'
	PathDirect Path = "direct" // driven_by = 'server'
)

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
