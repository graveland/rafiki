// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ColumnKind is a QueryResult column's value type. Money/percent formatting
// is NOT a fourth kind -- it is Column.Format, a rendering hint carried
// alongside a ColFloat value; the wire/value shape stays exactly three kinds.
type ColumnKind uint8

const (
	ColString ColumnKind = iota
	ColInt
	ColFloat
)

// Column describes one column of a QueryResult, declared up front by the
// query itself -- authoritative even for an empty result set, so a caller
// can inspect shape with zero rows in hand.
type Column struct {
	Name   string
	Kind   ColumnKind
	Format string // "" | "usd" | "pct" -- rendering hint only, never read off the wire
}

// Entry is one cell's value: a marker-interface union, the same shape as
// pkg/fundi/tools/registry.go's ContentBlock (interface{ isContentBlock() }),
// chosen for the same reason -- a type switch at each consumption site
// (proto encode, JSON encode, pkg/table formatting) is exhaustive, and a
// stray value that doesn't belong in a cell fails to compile rather than
// panicking three layers downstream in a renderer. Do NOT add a fourth
// concrete type without also handling it at every existing switch site --
// the first ones arrive with the wire/tool/CLI consumers (proto encode,
// tool encode, pkg/table formatting).
type Entry interface{ isEntry() }

type StringEntry string
type IntEntry int64
type FloatEntry float64

func (StringEntry) isEntry() {}
func (IntEntry) isEntry()    {}
func (FloatEntry) isEntry()  {}

// QueryResult is one catalogue query's answer: a declared schema plus rows
// matching it. len(row) == len(Columns) for every row a query returns --
// this is a convention query authors must uphold, not something this type
// enforces.
type QueryResult struct {
	Columns []Column
	Rows    [][]Entry
}

// Class is the admission decision for one named query, the same
// zero-value-denies shape as Scope itself: ClassUnset is the zero value and
// is inadmissible, so a query registered without a deliberate class simply
// does not run.
type Class uint8

const (
	ClassUnset Class = iota
	// ClassOwnerScoped is the common case: the query reads conversation
	// content and carries the caller's scope as an ownership predicate.
	// Under ScopeAll the predicate is dropped, never refused.
	ClassOwnerScoped
	// ClassGlobalFact is independent of anyone's content -- admissible
	// under any scope, including a denying one. No query uses this yet.
	ClassGlobalFact
	// ClassAdminOnly requires ScopeAll; refused otherwise. No query uses
	// this yet.
	ClassAdminOnly
)

// catalogQuery is one registered named query.
type catalogQuery struct {
	class Class
	run   func(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error)
}

// catalogue holds every registered named query, keyed by name. Query files
// added in later waves populate this from their own init() (see Wave 2's
// registerQuery calls) -- this task defines the map and the registration
// helper, and seeds no entries of its own.
var catalogue = map[string]catalogQuery{}

// registerQuery adds one named query to the catalogue. Called only from a
// query file's own init(); panics on a duplicate name, which is a
// programming error (two files claiming the same name) caught at process
// start, not a runtime condition any caller needs to handle.
func registerQuery(name string, class Class, run func(ctx context.Context, pool *pgxpool.Pool, scope Scope, f StatsFilter) (QueryResult, error)) {
	if _, exists := catalogue[name]; exists {
		panic("insights: duplicate query registration: " + name)
	}
	catalogue[name] = catalogQuery{class: class, run: run}
}

// Query is the ONE admission-checked entry point for the named-query
// catalogue; nothing else may call a catalogQuery.run directly. It refuses
// with ErrNotFound on an unknown name, a ClassUnset registration, a
// ClassAdminOnly query under a non-all scope, a ClassOwnerScoped query under
// an invalid scope, or an unrecognized Class -- folding "no such query" into
// the same not-found shape ConversationStats/Export already use for a scope
// miss, rather than inventing a new error shape here. The default arm of the
// admission switch is what makes a future Class constant added without an
// arm fail closed rather than run ungated.
func (i *Insights) Query(ctx context.Context, scope Scope, name string, f StatsFilter) (QueryResult, error) {
	q, ok := catalogue[name]
	if !ok || q.class == ClassUnset {
		return QueryResult{}, fmt.Errorf("query %q: %w", name, ErrNotFound)
	}
	switch q.class {
	case ClassAdminOnly:
		if !scope.all {
			return QueryResult{}, fmt.Errorf("query %q: %w", name, ErrNotFound)
		}
	case ClassGlobalFact:
		// No predicate; admissible under any scope, including a denying one.
	case ClassOwnerScoped:
		if !scope.valid() {
			return QueryResult{}, fmt.Errorf("query %q: %w", name, ErrNotFound)
		}
	default:
		return QueryResult{}, fmt.Errorf("query %q: %w", name, ErrNotFound)
	}
	return q.run(ctx, i.pool, scope, f)
}
