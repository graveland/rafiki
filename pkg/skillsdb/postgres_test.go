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
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/usersdb/postgres_test.go's testStore rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (skills.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_skills_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPostgresStore(pool), pool
}

func TestUpsertGetList(t *testing.T) {
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
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "body one" || got.Source != skills.CoreSource {
		t.Fatalf("got %+v", got)
	}

	if _, err := st.Get(ctx, "rafiki", "nope"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("missing skill: got %v, want ErrNotFound", err)
	}

	rows, err := st.List(ctx, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

// A disabled row frees its name, so an operator can add their own row under
// it. This is the whole override mechanism — if the partial index is wrong,
// this test is what catches it.
func TestDisabledNameCanBeReclaimed(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert core: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	rows, err := st.List(ctx, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Body != "our text" {
		t.Fatalf("got %+v, want exactly the override row", rows)
	}
}

// The core sync owns (namespace, source) and nothing else. An operator's row
// in the same namespace must survive a sync that does not mention it.
func TestReplaceNamespaceSourceLeavesOtherSourcesAlone(t *testing.T) {
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
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
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
	if _, ok := byName["operator-owned"]; !ok {
		t.Error("operator row was pruned by a core sync — ownership scoping is broken")
	}
}

// The sync ships a skill the operator disabled: the row must be refreshed in
// place, not resurrected as a second, enabled row — the partial index cannot
// see the disabled row, so a plain INSERT would silently bring the skill back.
func TestReplaceNamespaceSourceDoesNotResurrectADisabledCoreRow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "noisy", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "noisy", Body: "v2", Enabled: true},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found []skills.Record
	for _, r := range rows {
		if r.Name == "noisy" {
			found = append(found, r)
		}
	}
	if len(found) != 1 || found[0].Enabled || found[0].Body != "v2" {
		t.Fatalf("got %+v, want exactly one disabled row with refreshed body", found)
	}
}

// An enabled operator override shadows the core skill of the same name; the
// sync must leave its content alone — rows with a different source are never
// touched.
func TestReplaceNamespaceSourceDoesNotClobberAnEnabledOverride(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	if err := st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "model-selection", Body: "vendor text v2", Enabled: true},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows, err := st.List(ctx, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Body != "our text" {
		t.Fatalf("got %+v, want the operator override untouched by the sync", rows)
	}
}

// Upsert must not resurrect a row an operator disabled: re-syncing content is
// not permission to turn a skill back on.
func TestUpsertDoesNotReEnableADisabledRow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "noisy", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v2", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := st.Get(ctx, "rafiki", "noisy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled {
		t.Error("upsert re-enabled a disabled row")
	}
	if got.Body != "v2" {
		t.Errorf("body not updated: got %q, want %q", got.Body, "v2")
	}
}

// Re-enabling after the override machinery has run must flip exactly the
// disabled row — single row in, single enabled row out, no unique violation.
// The flow is the one the sync and an operator produce together: a core skill
// disabled to free its name, its content re-upserted over the disabled row
// (staying disabled), then the operator turns it back on.
func TestSetEnabledReEnablesAfterTheOverrideFlow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v1", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "noisy", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "noisy", Body: "v2", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "noisy", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	got, err := st.Get(ctx, "rafiki", "noisy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled || got.Body != "v2" {
		t.Fatalf("got %+v, want the single row re-enabled with fresh content", got)
	}
	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly one — the name must stay unique", len(rows))
	}
	// Already-enabled: nothing left to flip.
	if err := st.SetEnabled(ctx, "rafiki", "noisy", true); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("re-enable of an enabled row: got %v, want ErrNotFound", err)
	}
}

// Disabling in the override state (disabled core row + enabled override) must
// switch off the ENABLED row only — not issue the name-wide update that
// touches the disabled core row too.
func TestSetEnabledDisableTouchesOnlyTheEnabledRow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); err != nil {
		t.Fatalf("disable core: %v", err)
	}
	core, err := st.Get(ctx, "rafiki", "model-selection")
	if err != nil {
		t.Fatalf("get core: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); err != nil {
		t.Fatalf("disable override: %v", err)
	}

	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want both the core and the override row", len(rows))
	}
	for _, r := range rows {
		if r.Enabled {
			t.Errorf("%s row still enabled", r.Source)
		}
		if r.Source == skills.CoreSource && !r.UpdatedAt.Equal(core.UpdatedAt) {
			t.Errorf("disabled core row was touched by the override's disable: "+
				"updated_at moved from %v to %v", core.UpdatedAt, r.UpdatedAt)
		}
	}
	// Nothing enabled remains: a second disable has no row to flip.
	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("disable of an all-disabled name: got %v, want ErrNotFound", err)
	}
}

// Delete is the only management surface a disabled row has, so it removes the
// WHOLE name family — not just the enabled row an override left behind.
func TestDeleteRemovesTheWholeNameFamily(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "vendor text", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	if err := st.SetEnabled(ctx, "rafiki", "model-selection", false); err != nil {
		t.Fatalf("disable core: %v", err)
	}
	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	if err := st.Delete(ctx, "rafiki", "model-selection"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, "rafiki", "model-selection"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("get after delete: got %v, want ErrNotFound", err)
	}
	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %+v, want no rows left under the name", rows)
	}
	if err := st.Delete(ctx, "rafiki", "model-selection"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("delete of an absent name: got %v, want ErrNotFound", err)
	}
}

// The sync's DO UPDATE carries a source guard: a conflicting ENABLED row of a
// different source must be left exactly as it is, not rewritten with the
// sync's content. Unlike the clobber test above, no same-source disabled row
// exists here, so the INSERT (and its ON CONFLICT clause) actually runs.
func TestReplaceNamespaceSourceSkipsAnEnabledOverrideOfAnotherSource(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: "model-selection",
		Description: "my description", Body: "our text", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	if err := st.ReplaceNamespaceSource(ctx, "rafiki", skills.CoreSource, []skills.Record{
		{Namespace: "rafiki", Name: "model-selection",
			Description: "vendor description", Body: "vendor text v2", Enabled: true},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows, err := st.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want only the override row", len(rows))
	}
	got := rows[0]
	if got.Source != "manual" || got.Body != "our text" || got.Description != "my description" || !got.Enabled {
		t.Fatalf("got %+v, want the operator override untouched by the sync", got)
	}
}
