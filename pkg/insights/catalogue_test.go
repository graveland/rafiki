// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQueryUnknownNameReturnsNotFound(t *testing.T) {
	ins := &Insights{}
	_, err := ins.Query(context.Background(), ScopeAll(), "no-such-query", StatsFilter{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestQueryClassUnsetRefusesEvenIfRegistered(t *testing.T) {
	catalogue["__test_unset"] = catalogQuery{class: ClassUnset, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		t.Fatal("run must never be called for a ClassUnset registration")
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_unset")

	ins := &Insights{}
	_, err := ins.Query(context.Background(), ScopeAll(), "__test_unset", StatsFilter{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Class(99) is the admission switch's default arm -- the fail-closed property
// that keeps a future Class constant added without an admission arm from
// running ungated. Registering one under ScopeAll proves the arm fires: the
// query is known by name, so the not-found must come from the class refusal,
// never from run.
func TestQueryUnknownClassRefuses(t *testing.T) {
	catalogue["__test_unknown_class"] = catalogQuery{class: Class(99), run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		t.Fatal("run must never be called for an unrecognized class")
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_unknown_class")

	ins := &Insights{}
	_, err := ins.Query(context.Background(), ScopeAll(), "__test_unknown_class", StatsFilter{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestQueryOwnerScopedRefusesZeroValueScope(t *testing.T) {
	called := false
	catalogue["__test_owner_scoped"] = catalogQuery{class: ClassOwnerScoped, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		called = true
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_owner_scoped")

	ins := &Insights{}
	_, err := ins.Query(context.Background(), Scope{}, "__test_owner_scoped", StatsFilter{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if called {
		t.Fatal("run must not be called when scope is invalid")
	}
}

func TestQueryOwnerScopedRunsUnderValidScope(t *testing.T) {
	want := QueryResult{Columns: []Column{{Name: "x", Kind: ColInt}}, Rows: [][]Entry{{IntEntry(1)}}}
	catalogue["__test_owner_scoped_ok"] = catalogQuery{class: ClassOwnerScoped, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		return want, nil
	}}
	defer delete(catalogue, "__test_owner_scoped_ok")

	ins := &Insights{}
	got, err := ins.Query(context.Background(), ScopeOwner("someone"), "__test_owner_scoped_ok", StatsFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0][0] != IntEntry(1) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestQueryAdminOnlyRefusesNonAllScope(t *testing.T) {
	called := false
	catalogue["__test_admin_only"] = catalogQuery{class: ClassAdminOnly, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		called = true
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_admin_only")

	ins := &Insights{}
	_, err := ins.Query(context.Background(), ScopeOwner("bob"), "__test_admin_only", StatsFilter{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if called {
		t.Fatal("run must not be called under a non-all scope")
	}
}

func TestQueryAdminOnlyRunsUnderScopeAll(t *testing.T) {
	catalogue["__test_admin_only_ok"] = catalogQuery{class: ClassAdminOnly, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_admin_only_ok")

	ins := &Insights{}
	if _, err := ins.Query(context.Background(), ScopeAll(), "__test_admin_only_ok", StatsFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryOwnerScopedRunsUnderScopeAll(t *testing.T) {
	called := false
	catalogue["__test_owner_scoped_all"] = catalogQuery{class: ClassOwnerScoped, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		called = true
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_owner_scoped_all")

	ins := &Insights{}
	if _, err := ins.Query(context.Background(), ScopeAll(), "__test_owner_scoped_all", StatsFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("run must be called under ScopeAll: the predicate is dropped, never refused")
	}
}

func TestQueryPassesTheCallersScopeToRun(t *testing.T) {
	captured := Scope{}
	catalogue["__test_scope_passthrough"] = catalogQuery{class: ClassOwnerScoped, run: func(_ context.Context, _ *pgxpool.Pool, scope Scope, _ StatsFilter) (QueryResult, error) {
		captured = scope
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_scope_passthrough")

	ins := &Insights{}
	want := ScopeOwner("someone")
	if _, err := ins.Query(context.Background(), want, "__test_scope_passthrough", StatsFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != want {
		t.Fatalf("run saw scope %+v, want the caller's %+v", captured, want)
	}
}

func TestQueryGlobalFactRunsUnderZeroValueScope(t *testing.T) {
	catalogue["__test_global_fact"] = catalogQuery{class: ClassGlobalFact, run: func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		return QueryResult{}, nil
	}}
	defer delete(catalogue, "__test_global_fact")

	ins := &Insights{}
	if _, err := ins.Query(context.Background(), Scope{}, "__test_global_fact", StatsFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterQueryPanicsOnDuplicateName(t *testing.T) {
	registerQuery("__test_dup", ClassOwnerScoped, func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		return QueryResult{}, nil
	})
	defer delete(catalogue, "__test_dup")

	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate registration")
		}
	}()
	registerQuery("__test_dup", ClassOwnerScoped, func(context.Context, *pgxpool.Pool, Scope, StatsFilter) (QueryResult, error) {
		return QueryResult{}, nil
	})
}
