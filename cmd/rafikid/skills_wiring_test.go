// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/skillsdb"
)

// TestInlineSkillBodyServesTheEnabledRow pins the C1 seam end to end: a
// controller with a real Postgres skills store, the override state seeded
// exactly as an operator leaves it (core row disabled, replacement enabled),
// and the InlineSkillBody closure agentRuntimeOptions wires into the child.
// The closure must serve the ENABLED row's body — the inventory the child was
// spawned with lists that row, so a body from the disabled core row would be
// a different skill than the one the model asked for.
func TestInlineSkillBodyServesTheEnabledRow(t *testing.T) {
	pool := openTestPool(t)
	c := newTestController(t)
	c.skillStore = skillsdb.NewPostgresStore(pool)
	ctx := context.Background()

	// openTestPool shares the test DSN's database, so the rows this seeds are
	// cleaned by name and the name is unique per run.
	name := fmt.Sprintf("seam-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE namespace = 'rafiki' AND name = $1`, name)
	})

	if _, err := c.skillStore.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: name,
		Description: "core", Body: "core body", Source: skills.CoreSource, Enabled: true,
	}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	if err := c.skillStore.SetEnabled(ctx, "rafiki", name, false); err != nil {
		t.Fatalf("disable core: %v", err)
	}
	if _, err := c.skillStore.Upsert(ctx, skills.Record{
		Namespace: "rafiki", Name: name,
		Description: "ours", Body: "operator body", Source: "manual", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}, "c_skill_seam", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}

	// The spawn-time inventory carries the enabled row. The shared test DB
	// also holds the embedded core corpus, so the assertion is by name, not
	// by total count.
	var seen int
	for _, m := range ro.InlineSkills {
		if m.Name == name {
			seen++
			if m.Namespace != "rafiki" || m.Description != "ours" {
				t.Errorf("inventory entry for %q = %+v, want the enabled override row", name, m)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("inventory carries %d entries for %q, want exactly the enabled one", seen, name)
	}
	if ro.InlineSkillBody == nil {
		t.Fatal("InlineSkillBody was not wired")
	}
	body, err := ro.InlineSkillBody(ctx, "rafiki", name)
	if err != nil {
		t.Fatalf("inline body: %v", err)
	}
	if body != "operator body" {
		t.Fatalf("inline body = %q, want the ENABLED override row's body", body)
	}
}

// fakeSkillStore records what UpsertSkill's adapter sends and answers Get
// from a canned row, which is all the stamping tests need to see.
type fakeSkillStore struct {
	getRow    skills.Record
	getErr    error
	upsertErr error
	sawUpsert *skills.Record
}

func (f *fakeSkillStore) List(context.Context, bool) ([]skills.Record, error) { return nil, nil }
func (f *fakeSkillStore) Get(_ context.Context, ns, name string) (skills.Record, error) {
	if f.getErr != nil {
		return skills.Record{}, f.getErr
	}
	r := f.getRow
	r.Namespace, r.Name = ns, name
	return r, nil
}

func (f *fakeSkillStore) Upsert(_ context.Context, r skills.Record) (skills.Record, error) {
	f.sawUpsert = &r
	if f.upsertErr != nil {
		return skills.Record{}, f.upsertErr
	}
	return r, nil
}

func (f *fakeSkillStore) SetEnabled(context.Context, string, string, bool) error { return nil }
func (f *fakeSkillStore) Delete(context.Context, string, string) error           { return nil }
func (f *fakeSkillStore) ReplaceNamespaceSource(context.Context, string, string, []skills.Record) error {
	return nil
}

// shadowed_core_version was unreachable: nothing ever wrote it, so
// warnStaleOverrides — which compares the stamp against the running daemon —
// had no fact to compare. The adapter stamps the daemon's own version when
// the upsert replaces a core row, and the daemon's value wins over whatever
// the client claimed.
func TestUpsertSkillStampsVersionWhenOverridingACoreRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed string
	}{
		{"no client claim", ""},
		{"a client's claim is overridden", "0.0.1-made-up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSkillStore{getRow: skills.Record{
				Namespace: "rafiki", Name: "model-selection",
				Source: skills.CoreSource, Enabled: false,
			}}
			c := connectSkills{st: f, version: "v9.9.9-test"}

			_, err := c.UpsertSkill(context.Background(), connectapi.SkillRow{
				Namespace: "rafiki", Name: "model-selection",
				Body: "ours", Source: "manual", ShadowedCoreVersion: tc.claimed,
			})
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			if f.sawUpsert == nil {
				t.Fatal("store saw no upsert")
			}
			if got := f.sawUpsert.ShadowedCoreVersion; got != "v9.9.9-test" {
				t.Fatalf("shadowed_core_version = %q, want the daemon's version stamped", got)
			}
		})
	}
}

// An ordinary skill replaces nothing, so it must carry no phantom shadow
// version — the stamp is the fact "this row displaced a core skill". A Get
// that errors is likewise not evidence, and the upsert proceeds un-stamped
// rather than failing: stamping is advisory, never a gate.
func TestUpsertSkillStampsNothingForOrdinarySkills(t *testing.T) {
	for _, tc := range []struct {
		name   string
		getRow skills.Record
		getErr error
	}{
		{"replaces another manual row", skills.Record{Source: "manual"}, nil},
		{"replaces nothing (not found)", skills.Record{}, skills.ErrNotFound},
		{"store unreadable", skills.Record{}, errors.New("db is gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSkillStore{getRow: tc.getRow, getErr: tc.getErr}
			c := connectSkills{st: f, version: "v9.9.9-test"}

			out, err := c.UpsertSkill(context.Background(), connectapi.SkillRow{
				Namespace: "rafiki", Name: "fresh", Body: "b", Source: "manual",
			})
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			if got := out.ShadowedCoreVersion; got != "" {
				t.Fatalf("shadowed_core_version = %q, want empty", got)
			}
		})
	}
}

// The store's source-conflict sentinel must reach the Connect layer as
// connectapi's own, where the verb translates it to CodeAlreadyExists —
// leaving it unmapped would surface as CodeInternal with a store error string.
func TestUpsertSkillTranslatesSourceConflict(t *testing.T) {
	f := &fakeSkillStore{upsertErr: skills.ErrSourceConflict}
	c := connectSkills{st: f, version: "v1"}

	_, err := c.UpsertSkill(context.Background(), connectapi.SkillRow{
		Namespace: "rafiki", Name: "taken", Body: "b", Source: "manual",
	})
	if !errors.Is(err, connectapi.ErrSkillSourceConflict) {
		t.Fatalf("got %v, want connectapi.ErrSkillSourceConflict", err)
	}
}
