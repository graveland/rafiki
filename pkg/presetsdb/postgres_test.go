// SPDX-License-Identifier: Apache-2.0

package presetsdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/skillsdb/postgres_test.go's testStore, so this never touches a
// developer's real database.
func testStore(t *testing.T) (presets.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_presets_%d", time.Now().UnixNano())
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

// newOwner creates a real conversations.users row and returns its id:
// owner_user_id is a uuid FK to that table, so attribution must go through
// real users, exactly as production callers resolve it.
func newOwner(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(), username, false)
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u.ID
}

// The array allowlists are TRI-STATE end to end: Go nil must survive the
// round trip as SQL NULL and come back nil (the kind's default), while a
// non-nil empty slice must come back NON-NIL empty ("none") -- never
// collapsed into one another. reflect.DeepEqual(nil, []string{}) is false,
// so each case asserts pointer state explicitly, not just a len() that a
// collapsed value would also pass. Also covers ContextFiles nil/false/true
// and MaxCost nil vs 0.
func TestPresetStoreTriStateRoundTrip(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	boolPtr := func(b bool) *bool { return &b }
	floatPtr := func(f float64) *float64 { return &f }

	slices := []struct {
		name string
		want []string
	}{
		{"tri-nil", nil},
		{"tri-none", []string{}},
		{"tri-some", []string{"a", "b"}},
	}
	for _, s := range slices {
		if _, err := st.Put(ctx, "", presets.Record{
			Name: s.name, Kind: presets.KindFundi,
			Tools: s.want, Skills: s.want, MCPServers: s.want,
		}); err != nil {
			t.Fatalf("put %s: %v", s.name, err)
		}
	}
	for _, s := range slices {
		got, err := st.Get(ctx, "", s.name)
		if err != nil {
			t.Fatalf("get %s: %v", s.name, err)
		}
		if got.Labels == nil {
			t.Errorf("%s: Labels = nil, want a non-nil map (never nil after a read)", s.name)
		}
		for _, field := range []struct {
			label string
			slice []string
		}{
			{"tools", got.Tools},
			{"skills", got.Skills},
			{"mcp_servers", got.MCPServers},
		} {
			switch {
			case s.want == nil:
				if field.slice != nil {
					t.Errorf("%s.%s = %#v, want nil (the kind default)", s.name, field.label, field.slice)
				}
			case len(s.want) == 0:
				if field.slice == nil || len(field.slice) != 0 {
					t.Errorf("%s.%s = %#v, want NON-NIL and empty (\"none\")", s.name, field.label, field.slice)
				}
			default:
				if field.slice == nil || len(field.slice) != len(s.want) || field.slice[0] != s.want[0] || field.slice[1] != s.want[1] {
					t.Errorf("%s.%s = %#v, want exactly %#v", s.name, field.label, field.slice, s.want)
				}
			}
		}
	}

	contexts := []struct {
		name string
		want *bool
	}{
		{"ctx-nil", nil},
		{"ctx-false", boolPtr(false)},
		{"ctx-true", boolPtr(true)},
	}
	for _, c := range contexts {
		if _, err := st.Put(ctx, "", presets.Record{Name: c.name, Kind: presets.KindFundi, ContextFiles: c.want}); err != nil {
			t.Fatalf("put %s: %v", c.name, err)
		}
		got, err := st.Get(ctx, "", c.name)
		if err != nil {
			t.Fatalf("get %s: %v", c.name, err)
		}
		if (got.ContextFiles == nil) != (c.want == nil) {
			t.Errorf("%s: ContextFiles nil-ness = %v, want %v", c.name, got.ContextFiles == nil, c.want == nil)
		}
		if c.want != nil && (*got.ContextFiles != *c.want) {
			t.Errorf("%s: ContextFiles = %v, want %v", c.name, *got.ContextFiles, *c.want)
		}
	}

	costs := []struct {
		name string
		want *float64
	}{
		{"cost-nil", nil},
		{"cost-zero", floatPtr(0)},
	}
	for _, c := range costs {
		if _, err := st.Put(ctx, "", presets.Record{Name: c.name, Kind: presets.KindFundi, MaxCost: c.want}); err != nil {
			t.Fatalf("put %s: %v", c.name, err)
		}
		got, err := st.Get(ctx, "", c.name)
		if err != nil {
			t.Fatalf("get %s: %v", c.name, err)
		}
		if (got.MaxCost == nil) != (c.want == nil) {
			t.Errorf("%s: MaxCost nil-ness = %v, want %v", c.name, got.MaxCost == nil, c.want == nil)
		}
		if c.want != nil && (*got.MaxCost != *c.want) {
			t.Errorf("%s: MaxCost = %v, want %v", c.name, *got.MaxCost, *c.want)
		}
	}
}

// The nullable-by-absence text columns (provider, model, thinking, executor,
// system_prompt, append_system_prompt, written_by_child) encode "unset" as
// SQL NULL, never the empty string: Put maps Go "" through NULLIF and the
// read path COALESCEs NULL back to "". Get alone cannot prove the NULL half
// -- COALESCE hides a dropped NULLIF behind a read-back "" -- so the unset
// case also asserts the RAW columns are NULL through the pool.
func TestPresetStoreTextColumnsRoundTrip(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()

	cols := []struct {
		column string                        // conversations.presets column
		value  string                        // non-empty value that must round-trip exactly
		set    func(*presets.Record, string) // the Record field the column is written from
		get    func(presets.Record) string   // the Record field the column scans into
	}{
		{"provider", "openrouter",
			func(r *presets.Record, v string) { r.Provider = v },
			func(r presets.Record) string { return r.Provider }},
		{"model", "z-ai/glm-4.6",
			func(r *presets.Record, v string) { r.Model = v },
			func(r presets.Record) string { return r.Model }},
		{"thinking", "high",
			func(r *presets.Record, v string) { r.Thinking = v },
			func(r presets.Record) string { return r.Thinking }},
		{"executor", "env=work,os=linux",
			func(r *presets.Record, v string) { r.Executor = v },
			func(r presets.Record) string { return r.Executor }},
		{"system_prompt", "You are a reviewer.",
			func(r *presets.Record, v string) { r.SystemPrompt = v },
			func(r presets.Record) string { return r.SystemPrompt }},
		{"append_system_prompt", "Be terse.",
			func(r *presets.Record, v string) { r.AppendSystemPrompt = v },
			func(r presets.Record) string { return r.AppendSystemPrompt }},
		{"written_by_child", "spawn-1",
			func(r *presets.Record, v string) { r.WrittenByChild = v },
			func(r presets.Record) string { return r.WrittenByChild }},
	}

	// Unset: every field is the zero "" -- stored as SQL NULL, read back "".
	if _, err := st.Put(ctx, "", presets.Record{Name: "text-unset", Kind: presets.KindFundi}); err != nil {
		t.Fatalf("put text-unset: %v", err)
	}
	got, err := st.Get(ctx, "", "text-unset")
	if err != nil {
		t.Fatalf("get text-unset: %v", err)
	}
	for _, c := range cols {
		if v := c.get(got); v != "" {
			t.Errorf("text-unset.%s = %q, want \"\" (unset round-trips)", c.column, v)
		}
	}
	var unsetIsNull bool
	if err := pool.QueryRow(ctx, `SELECT
			provider IS NULL AND model IS NULL AND thinking IS NULL AND executor IS NULL
			AND system_prompt IS NULL AND append_system_prompt IS NULL AND written_by_child IS NULL
			FROM conversations.presets WHERE name = 'text-unset'`).Scan(&unsetIsNull); err != nil {
		t.Fatalf("read raw unset row: %v", err)
	}
	if !unsetIsNull {
		t.Error("unset text columns must be stored as SQL NULL, not '' -- a NULLIF is missing from Put")
	}

	// Non-empty: every column set at once (distinct values would also expose
	// a swapped NULLIF placeholder); each must come back exactly.
	full := presets.Record{Name: "text-full", Kind: presets.KindFundi}
	for _, c := range cols {
		c.set(&full, c.value)
	}
	if _, err := st.Put(ctx, "", full); err != nil {
		t.Fatalf("put text-full: %v", err)
	}
	got, err = st.Get(ctx, "", "text-full")
	if err != nil {
		t.Fatalf("get text-full: %v", err)
	}
	for _, c := range cols {
		if v := c.get(got); v != c.value {
			t.Errorf("text-full.%s = %q, want exactly %q", c.column, v, c.value)
		}
	}
}

// Put is append-only: two Puts under one name leave two rows, Get serves the
// second (higher id), and History returns both, newest first.
func TestPresetStoreAppendOnlyLatest(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "preset-append")

	first, err := st.Put(ctx, owner, presets.Record{Name: "review", Kind: presets.KindFundi, Description: "v1"})
	if err != nil {
		t.Fatalf("put first: %v", err)
	}
	second, err := st.Put(ctx, owner, presets.Record{Name: "review", Kind: presets.KindFundi, Description: "v2"})
	if err != nil {
		t.Fatalf("put second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("second Put returned id %d — an UPDATE happened, not an insert", first.ID)
	}

	got, err := st.Get(ctx, owner, "review")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != second.ID || got.Description != "v2" {
		t.Fatalf("get = %+v, want the second Put's row (id %d, description v2)", got, second.ID)
	}

	hist, err := st.History(ctx, owner, "review")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history = %d rows, want 2: %+v", len(hist), hist)
	}
	if hist[0].ID != second.ID || hist[1].ID != first.ID {
		t.Fatalf("history ids = [%d %d], want newest first [%d %d]", hist[0].ID, hist[1].ID, second.ID, first.ID)
	}
	if hist[0].DeletedAt != nil || hist[1].DeletedAt != nil {
		t.Errorf("live history rows must have nil DeletedAt: %+v", hist)
	}
}

// Delete stamps every live version in place: Get and List then find nothing,
// History still returns both rows (now with non-nil DeletedAt), and a fresh
// Put revives the name. A delete of an unknown name is ErrNotFound.
func TestPresetStoreDeleteHidesAndPutRevives(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "preset-del-revive")

	if _, err := st.Put(ctx, owner, presets.Record{Name: "seat", Kind: presets.KindFundi, Description: "v1"}); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, presets.Record{Name: "seat", Kind: presets.KindFundi, Description: "v2"}); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if err := st.Delete(ctx, owner, "seat"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, owner, "seat"); !errors.Is(err, presets.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	rows, err := st.List(ctx, owner, "")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("list after delete = %+v, want 0 rows", rows)
	}
	hist, err := st.History(ctx, owner, "seat")
	if err != nil {
		t.Fatalf("history after delete: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history after delete = %d rows, want both versions: %+v", len(hist), hist)
	}
	for _, h := range hist {
		if h.DeletedAt == nil {
			t.Errorf("history row id %d has nil DeletedAt after delete", h.ID)
		}
	}

	third, err := st.Put(ctx, owner, presets.Record{Name: "seat", Kind: presets.KindFundi, Description: "v3"})
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, err := st.Get(ctx, owner, "seat")
	if err != nil {
		t.Fatalf("get after re-put: %v", err)
	}
	if got.ID != third.ID || got.Description != "v3" {
		t.Fatalf("get after re-put = %+v, want the fresh row (id %d, v3)", got, third.ID)
	}

	if err := st.Delete(ctx, owner, "ghost"); !errors.Is(err, presets.ErrNotFound) {
		t.Fatalf("delete of unknown name = %v, want ErrNotFound", err)
	}
}

// Owner scoping is absolute: owner B and the unattributed bucket must never
// see owner A's preset, and "" sees only "" rows.
func TestPresetStoreOwnerScoping(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "preset-owner-a")
	ownerB := newOwner(t, pool, "preset-owner-b")

	a, err := st.Put(ctx, ownerA, presets.Record{Name: "shared", Kind: presets.KindFundi, Description: "a's"})
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := st.Put(ctx, ownerB, presets.Record{Name: "shared", Kind: presets.KindFundi, Description: "b's"})
	if err != nil {
		t.Fatalf("put b: %v", err)
	}
	unattr, err := st.Put(ctx, "", presets.Record{Name: "shared", Kind: presets.KindFundi, Description: "unattributed"})
	if err != nil {
		t.Fatalf("put unattributed: %v", err)
	}

	want := map[string]presets.Record{ownerA: a, ownerB: b, "": unattr}
	for owner, rec := range want {
		got, err := st.Get(ctx, owner, "shared")
		if err != nil {
			t.Fatalf("get %q: %v", owner, err)
		}
		if got.ID != rec.ID || got.OwnerUserID != owner || got.Description != rec.Description {
			t.Fatalf("get %q = %+v, want own row id %d: another owner's row must not leak", owner, got, rec.ID)
		}
		rows, err := st.List(ctx, owner, "")
		if err != nil {
			t.Fatalf("list %q: %v", owner, err)
		}
		if len(rows) != 1 || rows[0].ID != rec.ID {
			t.Fatalf("list %q = %+v, want exactly own row id %d", owner, rows, rec.ID)
		}
	}
}

// The prefix filter is a literal prefix match, not LIKE: names local:a,
// local:b, default:a and local_x filtered by "local:" return exactly the two
// local: names -- local_x's underscore must not act as a wildcard.
func TestPresetStoreListPrefix(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "preset-prefix")

	for _, name := range []string{"local:a", "local:b", "default:a", "local_x"} {
		if _, err := st.Put(ctx, owner, presets.Record{Name: name, Kind: presets.KindFundi}); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	rows, err := st.List(ctx, owner, "local:")
	if err != nil {
		t.Fatalf("list prefix: %v", err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if len(names) != 2 || names[0] != "local:a" || names[1] != "local:b" {
		t.Fatalf("list prefix %q = %v, want exactly [local:a local:b] ordered by name", "local:", names)
	}

	all, err := st.List(ctx, owner, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("list with empty prefix = %d rows, want all 4: %+v", len(all), all)
	}
}

// A raw INSERT bypasses Validate entirely: the table's own CHECK must refuse
// a claude preset carrying a tools allowlist, independent of the domain rule.
func TestPresetStoreCheckRejectsClaudeTools(t *testing.T) {
	_, pool := testStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.presets (name, kind, tools) VALUES ('x', 'claude', '{}')`); err == nil {
		t.Fatal("raw INSERT of a claude preset with tools = nil error, want the CHECK constraint to reject it")
	}
}

// TestPresetStoreScriptKindRoundTrip pins the script-kind preset: a clean
// script preset inserts and reads back, and one carrying an LLM-shaping knob
// is refused by the database's own CHECK (0040's script arm), not merely by
// pkg/presets.Validate — the DB is the last gate Validate might be bypassed
// by.
func TestPresetStoreScriptKindRoundTrip(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	if _, err := st.Put(ctx, "", presets.Record{
		Name: "script-roundtrip", Kind: presets.KindScript, MaxDepth: intPtr(1),
	}); err != nil {
		t.Fatalf("put a clean script preset: %v", err)
	}
	got, err := st.Get(ctx, "", "script-roundtrip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != presets.KindScript {
		t.Fatalf("kind = %q, want %q", got.Kind, presets.KindScript)
	}

	for _, bad := range []struct {
		field string
		rec   presets.Record
	}{
		{"model", presets.Record{Name: "script-bad-model", Kind: presets.KindScript, Model: "anthropic/x"}},
		{"provider", presets.Record{Name: "script-bad-provider", Kind: presets.KindScript, Provider: "anthropic"}},
		{"thinking", presets.Record{Name: "script-bad-thinking", Kind: presets.KindScript, Thinking: "high"}},
		{"tools", presets.Record{Name: "script-bad-tools", Kind: presets.KindScript, Tools: []string{"read"}}},
		{"system_prompt", presets.Record{Name: "script-bad-prompt", Kind: presets.KindScript, SystemPrompt: "sp"}},
	} {
		if _, err := st.Put(ctx, "", bad.rec); err == nil {
			t.Errorf("script preset with %s set was accepted; the CHECK must refuse it", bad.field)
		}
	}
}

func intPtr(i int) *int { return &i }
