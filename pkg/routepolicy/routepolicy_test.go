// SPDX-License-Identifier: Apache-2.0

package routepolicy

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// The store tests share the integration database, so every line a test writes
// is namespaced by test name — unique per test, stable across runs — and
// assertions look their lines up by name instead of assuming the whole table.

// TestRoutePolicyStoreNewestWins proves set-supersedes-set and the Active
// ordering: one row per line (the newest), lines ordered by model_line.
func TestRoutePolicyStoreNewestWins(t *testing.T) {
	c := assert.NewAborting(t)
	s, ctx := testStore(t)
	lineA := "testline/a-" + t.Name()
	lineB := "testline/b-" + t.Name()

	c.NoError(s.Set(ctx, lineA, "sort=price"), "Set lineA 1")
	c.NoError(s.Set(ctx, lineA, "sort=latency"), "Set lineA 2")
	c.NoError(s.Set(ctx, lineB, "quant=fp8+"), "Set lineB")

	active, err := s.Active(ctx)
	c.Require().NoError(err, "Active")
	a, ai, okA := findLine(active, lineA)
	b, bi, okB := findLine(active, lineB)
	c.Require().True(okA, "Active lost lineA")
	c.Require().True(okB, "Active lost lineB")
	c.Eq("sort=latency", a.Spec, "lineA's newest row wins, got %q", a.Spec)
	c.Eq("quant=fp8+", b.Spec, "lineB row")
	c.True(ai < bi, "Active not ordered by model_line: %d then %d", ai, bi)
}

// TestRoutePolicyStoreDeleteSupersedesSet is the delete side of
// newest-row-wins: a tombstone hides the live row beneath it, and a later Set
// makes the line live again (append-only; nothing is ever updated).
func TestRoutePolicyStoreDeleteSupersedesSet(t *testing.T) {
	c := assert.NewAborting(t)
	s, ctx := testStore(t)
	line := "testline/" + t.Name()

	c.NoError(s.Set(ctx, line, "quant=fp8+"), "Set")
	c.NoError(s.Delete(ctx, line), "Delete")
	active, err := s.Active(ctx)
	c.Require().NoError(err, "Active after delete")
	if _, _, ok := findLine(active, line); ok {
		t.Error("Active returned the line after its tombstone")
	}

	// The tombstone only wins while it is the newest row.
	c.NoError(s.Set(ctx, line, "sort=price"), "re-Set")
	active, err = s.Active(ctx)
	c.Require().NoError(err, "Active after re-set")
	got, _, ok := findLine(active, line)
	c.Require().True(ok, "re-set line did not come back")
	c.Eq("sort=price", got.Spec, "re-set row")
}

// TestRoutePolicyStoreRejectsBadSpec proves Set validates with
// routing.ParseSpec BEFORE writing: a spec the parser rejects leaves no row.
func TestRoutePolicyStoreRejectsBadSpec(t *testing.T) {
	c := assert.NewAborting(t)
	s, ctx := testStore(t)
	line := "testline/" + t.Name()

	c.Error(s.Set(ctx, line, "bogus=1"), "Set with an unknown key")
	active, err := s.Active(ctx)
	c.Require().NoError(err, "Active")
	if _, _, ok := findLine(active, line); ok {
		t.Error("a rejected spec still wrote a row")
	}
}

// TestRoutePolicyStoreSetRequiresLine pins Set's other guard: an empty model
// line is refused before anything is written.
func TestRoutePolicyStoreSetRequiresLine(t *testing.T) {
	c := assert.NewAborting(t)
	s, ctx := testStore(t)

	c.Error(s.Set(ctx, "", "sort=price"), "Set with an empty model line")
}

// TestRoutePolicyStoreDeleteMissing pins ErrNotFound: deleting a line that was
// never set, and deleting twice — the tombstone is itself the newest row, so
// the line has no live row to end.
func TestRoutePolicyStoreDeleteMissing(t *testing.T) {
	c := assert.NewAborting(t)
	s, ctx := testStore(t)
	line := "testline/" + t.Name()

	c.ErrorIs(s.Delete(ctx, line), ErrNotFound, "delete of a never-set line")
	c.NoError(s.Set(ctx, line, "sort=price"), "Set")
	c.NoError(s.Delete(ctx, line), "Delete")
	c.ErrorIs(s.Delete(ctx, line), ErrNotFound, "second delete")
}

// TestPolicyResolveLineOverGlobal is the merge the daemon depends on: the
// line row's quant rides on top of the global row's sort and nodata — all
// three keys in one resolved spec.
func TestPolicyResolveLineOverGlobal(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.Require().NoError(p.Load([]Row{
		{ModelLine: "*", Spec: "sort=price,nodata"},
		{ModelLine: "z-ai/glm-5.3", Spec: "quant=fp8+"},
	}), "Load")

	checkSpec(c, p.Resolve("z-ai/glm-5.3-flash"), "line over global (prefix match)",
		routing.SortPrice, []string{"fp8+"}, nil, true, false)
	checkSpec(c, p.Resolve("z-ai/glm-5.3"), "line over global (exact match)",
		routing.SortPrice, []string{"fp8+"}, nil, true, false)
}

// TestPolicyResolveLongestLineWins proves the longest matching line wins
// outright: the shorter matching row does not fill the winner's gaps, and an
// id that only shares the vendor segment matches nothing.
func TestPolicyResolveLongestLineWins(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.Require().NoError(p.Load([]Row{
		{ModelLine: "z-ai", Spec: "sort=price,quant=int8"},
		{ModelLine: "z-ai/glm-5.3", Spec: "sort=latency"},
	}), "Load")

	checkSpec(c, p.Resolve("z-ai/glm-5.3-flash"), "longest line wins, no gap-fill from z-ai",
		routing.SortLatency, nil, nil, false, false)
	checkSpec(c, p.Resolve("z-ai/glm-5.3"), "exact match on the longest line",
		routing.SortLatency, nil, nil, false, false)
	checkSpec(c, p.Resolve("z-ai/other-model"), `a vendor/ id does not match the bare line "z-ai"`,
		routing.SortInherit, nil, nil, false, false)
}

// TestPolicyResolveNoRows proves the zero outcome: no policy at all, or rows
// that match nothing — either way Resolve is a zero Spec.
func TestPolicyResolveNoRows(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.True(p.Resolve("z-ai/glm-5.3-flash").IsZero(), "empty policy resolved non-zero")
	c.Len(p.Rows(), 0, "empty policy has rows")

	c.Require().NoError(p.Load([]Row{{ModelLine: "z-ai/glm-5.3", Spec: "quant=fp8+"}}), "Load")
	c.True(p.Resolve("z-ai/glm-4.5").IsZero(), "an unmatched id resolved non-zero")
	c.True(p.Resolve("openrouter/z-ai/glm-4.5").IsZero(), "an unmatched provider-prefixed id resolved non-zero")
}

// TestPolicyResolveStripsProviderSegment pins the id-reduction rule: a
// three-segment id has its leading <provider>/ segment stripped before line
// matching; other shapes pass through unchanged.
func TestPolicyResolveStripsProviderSegment(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.Require().NoError(p.Load([]Row{{ModelLine: "z-ai/glm-5.3", Spec: "quant=fp8+"}}), "Load")

	checkSpec(c, p.Resolve("openrouter/z-ai/glm-5.3-flash"), "three segments: provider stripped",
		routing.SortInherit, []string{"fp8+"}, nil, false, false)
	checkSpec(c, p.Resolve("z-ai/glm-5.3-flash"), "two segments: nothing to strip",
		routing.SortInherit, []string{"fp8+"}, nil, false, false)

	c.Require().NoError(p.Load([]Row{{ModelLine: "a/b", Spec: "sort=price"}}), "Load a/b")
	checkSpec(c, p.Resolve("p/a/b"), "three segments over a two-segment line",
		routing.SortPrice, nil, nil, false, false)
	c.True(p.Resolve("x/p/a/b").IsZero(), "four segments are not stripped")
}

// TestPolicyLoadRejectsBadSpec proves a load is all-or-nothing: a row the
// parser rejects fails the whole load naming the line, and the previously
// loaded view survives untouched until a good load replaces it.
func TestPolicyLoadRejectsBadSpec(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.Require().NoError(p.Load([]Row{{ModelLine: "z-ai/glm-5.3", Spec: "sort=price"}}), "good Load")

	err := p.Load([]Row{{ModelLine: "z-ai/glm-5.3", Spec: "bogus=1"}})
	c.Require().Error(err, "Load with an unparseable spec")
	c.ErrorContains(err, "z-ai/glm-5.3", "the error names the offending line")

	checkSpec(c, p.Resolve("z-ai/glm-5.3-flash"), "failed load left the old view",
		routing.SortPrice, nil, nil, false, false)
	rows := p.Rows()
	c.Require().Len(rows, 1, "failed load left the old rows")

	// And a good load replaces everything, including to empty.
	c.Require().NoError(p.Load(nil), "empty Load")
	c.True(p.Resolve("z-ai/glm-5.3-flash").IsZero(), "cleared view resolved non-zero")
	c.Len(p.Rows(), 0, "cleared view has rows")
}

// TestPolicyRowsIsACopyOnASortedList pins Rows' contract: rows come back
// sorted by model line, and mutating the returned slice cannot reach the
// policy's view.
func TestPolicyRowsIsACopyOnASortedList(t *testing.T) {
	c := assert.NewCollecting(t)
	p := NewPolicy()
	c.Require().NoError(p.Load([]Row{
		{ModelLine: "z-ai/glm-5.3", Spec: "quant=fp8+"},
		{ModelLine: "*", Spec: "sort=price"},
	}), "Load")

	rows := p.Rows()
	c.Require().Len(rows, 2, "Rows")
	c.Eq("*", rows[0].ModelLine, "sorted: * before z-ai/glm-5.3")
	rows[0].Spec = "tampered"
	c.Eq("sort=price", p.Rows()[0].Spec, "Rows returned a copy")
}

// checkSpec asserts a resolved spec carries exactly the wanted keys — field
// by field, so the assertions hold whatever routing.Spec's struct comparison
// or constructor surface looks like.
func checkSpec(c *assert.C, got routing.Spec, label string, sort routing.Sort, quant, only []string, nodata, zdr bool) {
	c.Eq(sort, got.Sort, "%s: sort (spec %q)", label, got.String())
	c.True(slices.Equal(got.Quant, quant), "%s: quant = %v, want %v (spec %q)", label, got.Quant, quant, got.String())
	c.True(slices.Equal(got.Only, only), "%s: only = %v, want %v (spec %q)", label, got.Only, only, got.String())
	c.Eq(nodata, got.NoData, "%s: nodata (spec %q)", label, got.String())
	c.Eq(zdr, got.ZDR, "%s: zdr (spec %q)", label, got.String())
}

func testStore(t *testing.T) (Store, context.Context) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	return NewPostgres(pool), ctx
}

func findLine(rows []Row, line string) (Row, int, bool) {
	for i, r := range rows {
		if r.ModelLine == line {
			return r, i, true
		}
	}
	return Row{}, -1, false
}
