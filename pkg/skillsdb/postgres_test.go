// SPDX-License-Identifier: Apache-2.0

package skillsdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/usersdb/postgres_test.go's testStore rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (skills.Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_skills_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return NewPostgresStore(pool), pool
}

// The override state leaves two rows under one name: a disabled core row and
// an enabled override. Get serves the INLINE SKILL BODY path and `skills
// show`, so a row order that hands back the disabled core row serves content
// an operator deliberately switched off. The enabled row must win.
func TestGetPrefersTheEnabledRowInOverrideState(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "core text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable core")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	for i := 0; i < 10; i++ {
		got, err := st.Get(ctx, "rafiki", "model-selection")
		c.NoError(err, "get")
		c.False(got.Body != "our text" || !got.Enabled, "iteration %d: got body %q enabled=%v, want the ENABLED override row", i, got.Body, got.Enabled)
	}
}

func TestUpsertGetList(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "coordinating",
		Description: "how to run subagents", Body: "body one",
		Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.Get(ctx, "rafiki", "coordinating")
	c.NoError(err, "get")
	c.False(got.Body != "body one" || got.Source != skills.CoreSource, "got %+v", got)

	if _, err := st.Get(ctx, "rafiki", "nope"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("missing skill: got %v, want ErrNotFound", err)
	}

	rows, err := st.List(ctx, true)
	c.NoError(err, "list")
	c.Len(rows, 1, "got %d rows, want 1", len(rows))
}

// A disabled row frees its name, so an operator can add their own row under
// it. This is the whole override mechanism — if the partial index is wrong,
// this test is what catches it.
func TestDisabledNameCanBeReclaimed(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert core: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	rows, err := st.List(ctx, true)
	c.NoError(err, "list")
	c.False(len(rows) != 1 || rows[0].Body != "our text", "got %+v, want exactly the override row", rows)
}

// An upsert refreshes content; it does not take a name away from another
// source. Rewriting an enabled row's source would hand the name to the
// upserter while the old provenance — and the sync that owns it — silently
// loses its row, so a different source over an enabled row is refused, and
// the incumbent left byte-identical. The same source stays the normal
// replace path.
func TestUpsertRefusesSourceChangeOnEnabledRow(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	seed := skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "v1", Source: "manual", Enabled: true,
	}
	if _, err := st.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "hijacked", Source: "import:other", Enabled: true,
	})
	c.ErrorIs(err, skills.ErrSourceConflict, "cross-source upsert over enabled row: got")

	got, err := st.Get(ctx, "rafiki", "model-selection")
	c.NoError(err, "get")
	c.False(got.Source != "manual" || got.Body != "v1", "incumbent rewritten: %+v, want source manual body v1", got)

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "v2", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("same-source refresh: %v", err)
	}
	got, err = st.Get(ctx, "rafiki", "model-selection")
	c.NoError(err, "get after refresh")
	c.False(got.Body != "v2" || got.Source != "manual", "same-source refresh: %+v, want body v2 under source manual", got)
}

// Enabling in the two-row override state hits the partial unique index: one
// enabled row per name. That conflict is an ANSWER about the corpus (the name
// is taken), so it must surface as the typed sentinel — a raw 23505 would
// leave the Connect layer nothing to translate but driver text under
// CodeInternal. The escape is Delete, then let the sync reinsert core content.
func TestSetEnabledEnableInOverrideStateIsErrSourceConflict(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "core text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable core")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	err := st.SetEnabled(ctx, "rafiki", "model-selection", true)
	c.ErrorIs(err, skills.ErrSourceConflict, "enable in override state: got")
	// The corpus is unchanged: both rows survive, the override still enabled.
	rows, err := st.List(ctx, false)
	c.NoError(err, "list")
	c.Len(rows, 2, "got %d rows, want the core and override rows both intact", len(rows))
}

// The core sync owns (namespace, source) and nothing else. An operator's row
// in the same namespace must survive a sync that does not mention it.
func TestReplaceNamespaceSourceLeavesOtherSourcesAlone(t *testing.T) {
	c := assert.NewCollecting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	for _, r := range []skills.Record{
		{Namespace: "rafiki", Name: "gone-next-sync", Body: "x", Source: skills.CoreSource, Enabled: true},
		{Namespace: "rafiki", Name: "operator-owned", Body: "y", Source: "manual", Enabled: true},
	} {
		if _, err := st.Upsert(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name, err)
		}
	}

	err := st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "brand-new", Body: "z", Source: skills.CoreSource, Enabled: true},
	})
	c.Require().NoError(err, "replace")

	rows, err := st.List(ctx, false)
	c.Require().NoError(err, "list")
	byName := map[string]skills.Record{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if _, ok := byName["gone-next-sync"]; ok {
		t.Error("core row absent from want should have been pruned")
	}
	if _, ok := byName["brand-new"]; !ok {
		t.Error("core row present in want should have been inserted")
	}
	_, ok := byName["operator-owned"]
	c.True(ok, "operator row was pruned by a core sync — ownership scoping is broken")
}

// The sync ships a skill the operator disabled: the row must be refreshed in
// place, not resurrected as a second, enabled row — the partial index cannot
// see the disabled row, so a plain INSERT would silently bring the skill back.
func TestReplaceNamespaceSourceDoesNotResurrectADisabledCoreRow(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "noisy", false), "disable")
	c.NoError(st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "noisy", Body: "v2", Enabled: true},
	}), "replace")

	rows, err := st.List(ctx, false)
	c.NoError(err, "list")
	var found []skills.Record
	for _, r := range rows {
		if r.Name == "noisy" {
			found = append(found, r)
		}
	}
	c.False(len(found) != 1 || found[0].Enabled || found[0].Body != "v2", "got %+v, want exactly one disabled row with refreshed body", found)
}

// An enabled operator override shadows the core skill of the same name; the
// sync must leave its content alone — rows with a different source are never
// touched.
func TestReplaceNamespaceSourceDoesNotClobberAnEnabledOverride(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	c.NoError(st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "model-selection", Body: "vendor text v2", Enabled: true},
	}), "replace")

	rows, err := st.List(ctx, true)
	c.NoError(err, "list")
	c.False(len(rows) != 1 || rows[0].Body != "our text", "got %+v, want the operator override untouched by the sync", rows)
}

// Upsert must not resurrect a row an operator disabled: re-syncing content is
// not permission to turn a skill back on.
func TestUpsertDoesNotReEnableADisabledRow(t *testing.T) {
	c := assert.NewCollecting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.Require().NoError(st.SetEnabled(ctx, "rafiki", "noisy", false), "disable")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v2", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := st.Get(ctx, "rafiki", "noisy")
	c.Require().NoError(err, "get")
	c.False(got.Enabled, "upsert re-enabled a disabled row")
	c.Eq("v2", got.Body, "body not updated: got")
}

// Re-enabling after the override machinery has run must flip exactly the
// disabled row — single row in, single enabled row out, no unique violation.
// The flow is the one the sync and an operator produce together: a core skill
// disabled to free its name, its content re-upserted over the disabled row
// (staying disabled), then the operator turns it back on.
func TestSetEnabledReEnablesAfterTheOverrideFlow(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "noisy", false), "disable")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v2", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "noisy", true), "re-enable")

	got, err := st.Get(ctx, "rafiki", "noisy")
	c.NoError(err, "get")
	c.False(!got.Enabled || got.Body != "v2", "got %+v, want the single row re-enabled with fresh content", got)
	rows, err := st.List(ctx, false)
	c.NoError(err, "list")
	c.Len(rows, 1, "got %d rows, want exactly one — the name must stay unique", len(rows))
	// Already-enabled: nothing left to flip.
	c.ErrorIs(st.SetEnabled(ctx, "rafiki", "noisy", true), skills.ErrNotFound, "re-enable of an enabled row: got")
}

// Disabling in the override state (disabled core row + enabled override) must
// switch off the ENABLED row only — not issue the name-wide update that
// touches the disabled core row too.
func TestSetEnabledDisableTouchesOnlyTheEnabledRow(t *testing.T) {
	c := assert.NewCollecting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c.Require().NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable core")
	core, err := st.Get(ctx, "rafiki", "model-selection")
	c.Require().NoError(err, "get core")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	c.Require().NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable override")

	rows, err := st.List(ctx, false)
	c.Require().NoError(err, "list")
	c.Require().Len(rows, 2, "got %d rows, want both the core and the override row", len(rows))
	for _, r := range rows {
		c.False(r.Enabled, "%s row still enabled", r.Source)
		if r.Source == skills.CoreSource && !r.UpdatedAt.Equal(core.UpdatedAt) {
			t.Errorf("disabled core row was touched by the override's disable: "+
				"updated_at moved from %v to %v", core.UpdatedAt, r.UpdatedAt)
		}
	}
	// Nothing enabled remains: a second disable has no row to flip.
	c.Require().ErrorIs(st.SetEnabled(ctx, "rafiki", "model-selection", false), skills.ErrNotFound, "disable of an all-disabled name: got")
}

// Delete is the only management surface a disabled row has, so it removes the
// WHOLE name family — not just the enabled row an override left behind.
func TestDeleteRemovesTheWholeNameFamily(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c.NoError(st.SetEnabled(ctx, "rafiki", "model-selection", false), "disable core")
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	c.NoError(st.Delete(ctx, "rafiki", "model-selection"), "delete")
	if _, err := st.Get(ctx, "rafiki", "model-selection"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("get after delete: got %v, want ErrNotFound", err)
	}
	rows, err := st.List(ctx, false)
	c.NoError(err, "list")
	c.Empty(rows, "got")
	c.ErrorIs(st.Delete(ctx, "rafiki", "model-selection"), skills.ErrNotFound, "delete of an absent name: got")
}

// The sync's DO UPDATE carries a source guard: a conflicting ENABLED row of a
// different source must be left exactly as it is, not rewritten with the
// sync's content. Unlike the clobber test above, no same-source disabled row
// exists here, so the INSERT (and its ON CONFLICT clause) actually runs.
func TestReplaceNamespaceSourceSkipsAnEnabledOverrideOfAnotherSource(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Description: "my description", Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	c.NoError(st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "model-selection",
			Description: "vendor description", Body: "vendor text v2", Enabled: true},
	}), "replace")

	rows, err := st.List(ctx, false)
	c.NoError(err, "list")
	c.Len(rows, 1, "got %d rows, want only the override row", len(rows))
	got := rows[0]
	c.False(got.Source != "manual" || got.Body != "our text" || got.Description != "my description" || !got.Enabled, "got %+v, want the operator override untouched by the sync", got)
}
