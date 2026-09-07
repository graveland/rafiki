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
