// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/users"
)

// The two conversationReader constructors are the binding story for the
// conversation_search/export tools: the scope they bake in is what keeps a
// caller inside its own corpus. Scope is unexported-fielded (a struct literal
// cannot construct a valid one outside pkg/insights), so equality against the
// constructors is the observable -- zero value denies, ScopeAll admits all.
func TestNewControllerConversationReaderScopesAnEmptyOwnerToDeny(t *testing.T) {
	if r := newControllerConversationReader(nil, ""); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous spawn scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
	if r := newControllerConversationReader(nil, "u-owner"); r.scope != insights.ScopeOwner("u-owner") {
		t.Errorf("owned spawn scope = %+v, want ScopeOwner", r.scope)
	}
}

func TestNewMCPConversationReaderScopePerIdentity(t *testing.T) {
	if r := newMCPConversationReader(nil, users.Identity{IsAdmin: true}); r.scope != insights.ScopeAll() {
		t.Errorf("admin caller scope = %+v, want ScopeAll", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{UserID: "u-alice"}); r.scope != insights.ScopeOwner("u-alice") {
		t.Errorf("user caller scope = %+v, want ScopeOwner", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{}); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous caller scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
}

// TestRunQueryMapsCatalogueEntriesAndColumns covers the insights.QueryResult
// onto tools.CatalogueResult mapping inside RunQuery. It needs no controller
// (the mapping is factored into catalogueResult) because conversationReader's
// ctrl is the concrete *Controller -- there is no fake to bind, and the two
// scope-construction tests above are the level the nil-ctrl readers support.
func TestRunQueryMapsCatalogueEntriesAndColumns(t *testing.T) {
	res, err := catalogueResult(insights.QueryResult{
		Columns: []insights.Column{
			{Name: "tool", Kind: insights.ColString},
			{Name: "calls", Kind: insights.ColInt},
			{Name: "coverage", Kind: insights.ColFloat, Format: "pct"},
		},
		Rows: [][]insights.Entry{
			{insights.StringEntry("bash"), insights.IntEntry(3), insights.FloatEntry(0.75)},
		},
	})
	if err != nil {
		t.Fatalf("catalogueResult: %v", err)
	}
	if len(res.Columns) != 3 ||
		res.Columns[0].Kind != "string" || res.Columns[1].Kind != "int" ||
		res.Columns[2].Kind != "float" || res.Columns[2].Format != "pct" {
		t.Errorf("columns = %+v, want the three kinds mapped and Format carried", res.Columns)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 3 {
		t.Fatalf("rows = %+v, want one row of three cells", res.Rows)
	}
	str, i, fl := res.Rows[0][0], res.Rows[0][1], res.Rows[0][2]
	if str.Str != "bash" || str.IsInt || str.IsFloat {
		t.Errorf("cell 0 = %+v, want Str only", str)
	}
	if !i.IsInt || i.Int != 3 || i.IsFloat {
		t.Errorf("cell 1 = %+v, want Int only", i)
	}
	if !fl.IsFloat || fl.Float != 0.75 || fl.IsInt {
		t.Errorf("cell 2 = %+v, want Float only", fl)
	}

	// An Entry outside the three concrete types cannot be built outside
	// pkg/insights (isEntry is unexported), but a nil cell reaches the same
	// default arm: fail loudly rather than render an empty cell.
	if _, err := catalogueResult(insights.QueryResult{
		Rows: [][]insights.Entry{{nil}},
	}); err == nil {
		t.Error("catalogueResult with a nil Entry returned no error")
	}
}

// TestConversationReaderRunQueryMapsScope observes the scope a reader was
// built with reaching the call. ctrl is the concrete *Controller (no fake
// exists), so this runs against a real pool: the zero-value scope must be
// refused by insights.Query's admission switch (ClassOwnerScoped under an
// invalid scope answers not-found), while an owner scope -- whatever it
// owns, here nothing -- runs and returns the query's declared columns.
func TestConversationReaderRunQueryMapsScope(t *testing.T) {
	pool := openTestPool(t)
	dir := testSocketDir(t)
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")
	for _, d := range []string{stateDir, logsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdirall %s: %v", d, err)
		}
	}
	ctrl := NewController(childstore.New(), stateDir, logsDir, filepath.Join(dir, "c.sock"), nil, pool, nil, t.Context(), nil, nil, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ctrl.ShutdownAllChildren(ctx, time.Second, time.Second); err != nil {
			t.Logf("cleanup: ShutdownAllChildren: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The zero-value Scope denies: an anonymous spawn's reader must not be
	// able to run a ClassOwnerScoped query at all.
	anon := newControllerConversationReader(ctrl, "")
	if _, err := anon.RunQuery(ctx, "tools", tools.CatalogueFilter{}); err == nil {
		t.Error("RunQuery with the zero-value scope returned no error; the deny-all scope did not reach the admission switch")
	}

	// An owner scope runs: no conversations carry this owner id, so the
	// result is empty -- but the declared columns come back regardless,
	// which is what proves the query executed rather than erroring.
	owned := newControllerConversationReader(ctrl, "00000000-0000-0000-0000-0000000000aa")
	res, err := owned.RunQuery(ctx, "tools", tools.CatalogueFilter{})
	if err != nil {
		t.Fatalf("RunQuery with an owner scope: %v", err)
	}
	if len(res.Columns) != 6 || res.Columns[0].Name != "tool" || res.Columns[1].Name != "calls" ||
		res.Columns[2].Name != "ok" || res.Columns[3].Name != "errors" ||
		res.Columns[4].Name != "unmatched" || res.Columns[5].Name != "conversations" {
		t.Errorf("columns = %+v, want the tools query's declared schema", res.Columns)
	}
	if len(res.Rows) != 0 {
		t.Errorf("rows = %+v, want none for an owner that owns nothing", res.Rows)
	}

	// An unknown name is refused even under a valid scope.
	if _, err := owned.RunQuery(ctx, "no-such-query", tools.CatalogueFilter{}); err == nil {
		t.Error("RunQuery with an unknown name returned no error")
	}
}
