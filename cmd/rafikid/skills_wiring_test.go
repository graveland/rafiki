// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/skillsdb"

	"github.com/multigres/testkit/assert"
)

// TestInlineSkillBodyServesTheSpawnTimeInventory pins the C1 seam end to end: a
// controller with a real Postgres skills store, the InlineSkillBody closure
// agentRuntimeOptions wires into the child, and the inventory it was spawned
// with. The closure answers from the rows that inventory named — by id, not by
// re-looking-up a name — so a skill that appeared after the spawn (or one that
// was disabled) is ErrNotFound rather than a body the model was never offered.
func TestInlineSkillBodyServesTheSpawnTimeInventory(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	c := newTestController(t)
	c.skillStore = skillsdb.NewPostgresStore(pool)
	ctx := context.Background()

	// openTestPool shares the test DSN's database, so the rows this seeds are
	// cleaned by source and both the namespace and the source are per-run
	// unique — the shared database is never reset between runs.
	run := time.Now().UnixNano()
	ns := fmt.Sprintf("seam-%d", run)
	key := skills.SetKey{Source: fmt.Sprintf("seam-source-%d", run)}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, key.Source)
	})

	put := func(want ...skills.Record) {
		t.Helper()
		if _, err := c.skillStore.PutSet(ctx, key, want); err != nil {
			t.Fatalf("PutSet: %v", err)
		}
	}
	rec := func(name, body string) skills.Record {
		return skills.Record{Namespace: ns, Name: name, Description: name + " description", Body: body}
	}

	put(rec("before", "before body"))

	ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}, "c_skill_seam", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions")

	// The spawn-time inventory names the row, and carries its id — the key the
	// closure resolves with. The shared test DB also holds the embedded core
	// corpus, so the assertion is by name, not by total count.
	var seen int
	for _, m := range ro.InlineSkills {
		if m.Name != "before" || m.Namespace != ns {
			continue
		}
		seen++
		ck.False(m.ID == "", "inventory entry for %q carries no id", m.Name)
		ck.Eq("before description", m.Description, "inventory description")
	}
	ck.Require().Eq(1, seen, "inventory carries %d entries for %q, want exactly one", seen, "before")

	ck.Require().NotNil(ro.InlineSkillBody, "InlineSkillBody was not wired")
	body, dir, err := ro.InlineSkillBody(ctx, beforeMeta(ro.InlineSkills, ns, "before"))
	ck.Require().NoError(err, "inline body")
	ck.Require().Eq("before body", body, "inline body")
	ck.Eq("", dir, "a skill with no files must not name a directory, got")

	// A skill written after the spawn is not in the inventory, so the child was
	// never offered it: the inventory is what the model can name, and it is
	// fixed at spawn.
	put(rec("before", "before body"), rec("after", "after body"))
	for _, m := range ro.InlineSkills {
		ck.False(m.Namespace == ns && m.Name == "after",
			"a skill written after the spawn reached the spawn-time inventory")
	}

	// An id the store does not hold is ErrNotFound, never an empty body: the
	// model asked for a row that is gone.
	_, _, err = ro.InlineSkillBody(ctx, skills.SkillMeta{Namespace: ns, Name: "after", ID: "00000000-0000-0000-0000-000000000000"})
	ck.ErrorIs(err, skills.ErrNotFound, "a row the store no longer holds: got")
}

// beforeMeta finds one entry of the spawn-time inventory by name. The shared
// test DB holds other rows too, so a test that wants the exact meta the child
// was offered has to read it back from the inventory rather than build one.
func beforeMeta(metas []skills.SkillMeta, namespace, name string) skills.SkillMeta {
	for _, m := range metas {
		if m.Namespace == namespace && m.Name == name {
			return m
		}
	}
	return skills.SkillMeta{Namespace: namespace, Name: name}
}

// TestInlineSkillNoExecutorNote pins the wording a model sees when a skill has
// supporting files and there is no machine to put them on. The body still
// arrives — the instructions are the point — and the note stops the model
// looking for files that are not there.
func TestInlineSkillNoExecutorNote(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &fakeSkillStore{
		rows: []skills.Record{{
			ID: "row-files", Namespace: "rafiki", Name: "with-files", Body: "BODY", HasFiles: true,
		}},
		files: map[string][]skills.File{"row-files": {{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")}}},
	}
	body, dir, err := inlineSkillBody(store, nil)(context.Background(),
		skills.SkillMeta{Namespace: "rafiki", Name: "with-files", ID: "row-files"})
	ck.Require().NoError(err, "inline body with no executor")
	ck.Eq("", dir, "an unmaterialized skill must not name a directory, got")
	ck.Eq("BODY\n\n"+noSkillFilesNote, body, "body")
	ck.Eq("(This skill has supporting files, but this agent has no executor to read them from.)",
		noSkillFilesNote, "the note's wording is what the model reads")
}

// TestInlineSkillMaterializeErrorSurfaces: a failure to write the files is the
// TOOL's error. Swallowing it would hand the model a body with no directory and
// no note, which reads as "this skill has no files".
func TestInlineSkillMaterializeErrorSurfaces(t *testing.T) {
	ck := assert.NewCollecting(t)
	boom := errors.New("executor refused the tree")
	store := &fakeSkillStore{
		rows: []skills.Record{{
			ID: "row-files", Namespace: "rafiki", Name: "with-files", Body: "BODY", HasFiles: true,
		}},
		files: map[string][]skills.File{"row-files": {{Path: "run.sh", Content: []byte("x")}}},
	}
	_, _, err := inlineSkillBody(store, func(context.Context, string, []skills.File) (string, bool, error) {
		return "", false, boom
	})(context.Background(), skills.SkillMeta{Namespace: "rafiki", Name: "with-files", ID: "row-files"})
	ck.Require().ErrorIs(err, boom, "a materialize failure must surface, got")
}

// TestInlineSkillMaterializedDirIsReturned: when the files land somewhere the
// child can read, the directory comes back with the body so the tool can name
// it.
func TestInlineSkillMaterializedDirIsReturned(t *testing.T) {
	ck := assert.NewCollecting(t)
	const dir = "/cache/skill-cache/deadbeef"
	store := &fakeSkillStore{
		rows: []skills.Record{{
			ID: "row-files", Namespace: "rafiki", Name: "with-files", Body: "BODY", HasFiles: true,
		}},
		files: map[string][]skills.File{"row-files": {{Path: "run.sh", Content: []byte("x")}}},
	}
	var gotHash string
	body, gotDir, err := inlineSkillBody(store, func(_ context.Context, treeHash string, files []skills.File) (string, bool, error) {
		gotHash = treeHash
		ck.Eq(1, len(files), "materializer got %d file(s)", len(files))
		return dir, true, nil
	})(context.Background(), skills.SkillMeta{Namespace: "rafiki", Name: "with-files", ID: "row-files"})
	ck.Require().NoError(err, "inline body")
	ck.Eq("BODY", body, "body")
	ck.Eq(dir, gotDir, "dir")
	ck.Eq(skills.TreeHash(store.files["row-files"]), gotHash, "materializer got tree hash")
}

// TestInlineSkillUnboundExecutorGetsTheNote: a child whose executor has not
// connected yet has nowhere to write the files, so it gets the note rather than
// an error. The next skill call, after it binds, materializes.
func TestInlineSkillUnboundExecutorGetsTheNote(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &fakeSkillStore{
		rows: []skills.Record{{
			ID: "row-files", Namespace: "rafiki", Name: "with-files", Body: "BODY", HasFiles: true,
		}},
		files: map[string][]skills.File{"row-files": {{Path: "run.sh", Content: []byte("x")}}},
	}
	body, dir, err := inlineSkillBody(store, func(context.Context, string, []skills.File) (string, bool, error) {
		return "", false, nil
	})(context.Background(), skills.SkillMeta{Namespace: "rafiki", Name: "with-files", ID: "row-files"})
	ck.Require().NoError(err, "inline body")
	ck.Eq("", dir, "dir")
	ck.Eq("BODY\n\n"+noSkillFilesNote, body, "body")
}

// TestSkillsWiringUsesResolve pins that a child's inventory is the RESOLVED
// set, not the store's rows: an operator's override of a core skill reaches
// their child AS the override, and the core row it shadows does not come along
// behind it.
//
// The store is read with enabledOnly=false deliberately. A disabled override is
// how an operator switches a core skill OFF, and it shadows the core row by
// merely existing — so resolving an enabled-only list would serve the core
// skill the operator just turned off. The second half of this test is that
// case.
func TestSkillsWiringUsesResolve(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	c := newTestController(t)
	c.skillStore = skillsdb.NewPostgresStore(pool)
	ctx := context.Background()

	// The shared test DB is never reset, so every row this seeds is per-run
	// unique and cleaned by name.
	run := time.Now().UnixNano()
	name := fmt.Sprintf("wiring-%d", run)
	owner := seedControllerUser(t, pool, fmt.Sprintf("wiring-owner-%d", run))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE name = $1`, name)
	})

	// Written directly rather than through PutSet: PutSet owns a whole SOURCE,
	// and this test needs one row in a source it does not own (the core
	// corpus). Only one row per (namespace, name) may be ENABLED — that is how
	// an override replaces a core skill — so the core row starts disabled.
	var coreID, userID string
	ck.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.skills (namespace, name, description, body, source, enabled)
		 VALUES ('rafiki', $1, 'core description', 'core body', $2, FALSE)
		 RETURNING id::text`, name, skills.CoreSource).Scan(&coreID), "seed core row")
	ck.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.skills (namespace, name, description, body, source, owner_user_id, enabled)
		 VALUES ('rafiki', $1, 'user description', 'user body', $2, $3::uuid, TRUE)
		 RETURNING id::text`, name, fmt.Sprintf("wiring-source-%d", run), owner).Scan(&userID), "seed user row")

	spawn := func() fundi.RuntimeOptions {
		t.Helper()
		ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
			Kind:  protocol.KindFundi,
			Cwd:   t.TempDir(),
			Model: "anthropic/claude-sonnet-4-5",
		}, "c_skill_resolve", false, "", owner)
		ck.Require().NoError(err, "agentRuntimeOptions")
		return ro
	}
	inventory := func(ro fundi.RuntimeOptions) []skills.SkillMeta {
		var out []skills.SkillMeta
		for _, m := range ro.InlineSkills {
			if m.Namespace == "rafiki" && m.Name == name {
				out = append(out, m)
			}
		}
		return out
	}

	ro := spawn()
	seen := inventory(ro)
	ck.Require().Eq(1, len(seen), "inventory carries %d entries for %s, want exactly one", len(seen), name)
	ck.Eq(userID, seen[0].ID, "the inventory entry must be the owner's override, got id")
	ck.Eq("user description", seen[0].Description, "inventory description")

	// And the body the child is served is the override's, not the core row's.
	body, _, err := ro.InlineSkillBody(ctx, seen[0])
	ck.Require().NoError(err, "inline body")
	ck.Eq("user body", body, "inline body")

	// The other direction: an operator disabling their override must hide the
	// core skill entirely, which is only possible when the spawn resolves the
	// DISABLED rows too.
	_, err = pool.Exec(ctx, `UPDATE conversations.skills SET enabled = FALSE WHERE id = $1::uuid`, userID)
	ck.Require().NoError(err, "disable the user row")
	_, err = pool.Exec(ctx, `UPDATE conversations.skills SET enabled = TRUE WHERE id = $1::uuid`, coreID)
	ck.Require().NoError(err, "re-enable the core row")

	ck.Eq(0, len(inventory(spawn())),
		"a disabled override must hide the core skill it shadows, and an enabled-only read cannot see it")
}

// fakeSkillStore records what the adapter asked for. List answers from rows;
// every other method records the id it was handed, which is the property these
// tests care about — a verb that passed a (namespace, name) would reach the
// store with a key that is only unique within a layer.
type fakeSkillStore struct {
	rows      []skills.Record
	files     map[string][]skills.File
	listErr   error
	getErr    error
	filesErr  error
	deleteErr error
	enableErr error

	gotID     string
	deletedID string
	enabledID string
	enabledTo bool
}

func (f *fakeSkillStore) List(context.Context, bool) ([]skills.Record, error) {
	return f.rows, f.listErr
}

func (f *fakeSkillStore) Get(_ context.Context, id string) (skills.Record, error) {
	f.gotID = id
	if f.getErr != nil {
		return skills.Record{}, f.getErr
	}
	for _, r := range f.rows {
		if r.ID == id {
			r.Files = f.files[id]
			return r, nil
		}
	}
	return skills.Record{}, skills.ErrNotFound
}

func (f *fakeSkillStore) Files(context.Context, []string) (map[string][]skills.File, error) {
	return f.files, f.filesErr
}

func (f *fakeSkillStore) PutSet(context.Context, skills.SetKey, []skills.Record) (bool, error) {
	return false, nil
}

func (f *fakeSkillStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	f.enabledID, f.enabledTo = id, enabled
	return f.enableErr
}

func (f *fakeSkillStore) Delete(_ context.Context, id string) error {
	f.deletedID = id
	return f.deleteErr
}

// A name is unique only within a layer, so a verb that takes (namespace, name)
// has to pick one: the caller's own row, then the global non-core row, then the
// core row. Another user's row is not addressable from here at all. GetSkill is
// the read that resolves the same way every write does — the writes' own source
// rules are pinned separately (TestSkillTargetPrefersUserLayer).
func TestConnectSkillsResolvesOwnRowThenGlobalThenCore(t *testing.T) {
	const (
		coreRow   = "core-id"
		globalRow = "global-id"
		ownRow    = "own-id"
		otherRow  = "other-id"
	)
	row := func(id, source, owner string) skills.Record {
		return skills.Record{ID: id, Namespace: "rafiki", Name: "s", Source: source, OwnerUserID: owner}
	}
	userCtx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u1", Username: "brent", Via: server.ProvenanceUser,
	})

	for _, tc := range []struct {
		name    string
		rows    []skills.Record
		ctx     context.Context
		wantID  string
		wantErr error
	}{
		{
			name: "own row wins over global and core",
			rows: []skills.Record{
				row(coreRow, skills.CoreSource, ""),
				row(globalRow, "manual", ""),
				row(ownRow, "repo:mine", "u1"),
			},
			ctx:    userCtx,
			wantID: ownRow,
		},
		{
			name: "global non-core row before the core row",
			rows: []skills.Record{
				row(coreRow, skills.CoreSource, ""),
				row(globalRow, "manual", ""),
			},
			ctx:    userCtx,
			wantID: globalRow,
		},
		{
			name:   "core row when nothing else holds the name",
			rows:   []skills.Record{row(coreRow, skills.CoreSource, "")},
			ctx:    userCtx,
			wantID: coreRow,
		},
		{
			name:   "no identity resolves the global and core layers only",
			rows:   []skills.Record{row(coreRow, skills.CoreSource, ""), row(ownRow, "repo:mine", "u1")},
			ctx:    context.Background(),
			wantID: coreRow,
		},
		{
			name:    "another user's row is not addressable",
			rows:    []skills.Record{row(otherRow, "repo:theirs", "u2")},
			ctx:     userCtx,
			wantErr: connectapi.ErrSkillNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSkillStore{rows: tc.rows}
			c := connectSkills{st: f}
			if _, err := c.GetSkill(tc.ctx, "rafiki", "s"); tc.wantErr != nil {
				assert.NewAborting(t).ErrorIs(err, tc.wantErr, "GetSkill: got")
				return
			} else if err != nil {
				t.Fatalf("GetSkill: %v", err)
			}
			assert.NewAborting(t).Eq(tc.wantID, f.gotID, "GetSkill reached the store with id")
		})
	}
}

// GetSkill and SetSkillEnabled resolve the same way and then act by id, so a
// caller can never flip a row in a layer it does not own by naming it.
func TestConnectSkillsActsByIdAfterResolving(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "s", Source: skills.CoreSource},
		{ID: "own-id", Namespace: "rafiki", Name: "s", Source: "repo:mine", OwnerUserID: "u1"},
	}}
	ctx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u1", Username: "brent", Via: server.ProvenanceUser,
	})
	c := connectSkills{st: f}

	got, err := c.GetSkill(ctx, "rafiki", "s")
	ck.Require().NoError(err, "GetSkill")
	ck.Eq("own-id", f.gotID, "GetSkill fetched by id")
	ck.Eq("rafiki", got.Namespace, "GetSkill namespace")
	ck.Eq("s", got.Name, "GetSkill name")

	ck.Require().NoError(c.SetSkillEnabled(ctx, "rafiki", "s", false), "SetSkillEnabled")
	ck.Eq("own-id", f.enabledID, "SetSkillEnabled reached the store with id")
	ck.False(f.enabledTo, "SetSkillEnabled sent enabled=true")
}

// A store read failure is "I could not check", never "no such skill": it must
// surface as an error rather than as NotFound, which a caller would read as the
// skill being absent.
func TestConnectSkillsListFailureIsNotNotFound(t *testing.T) {
	boom := errors.New("db is gone")
	c := connectSkills{st: &fakeSkillStore{listErr: boom}}

	_, err := c.GetSkill(context.Background(), "rafiki", "s")
	assert.NewAborting(t).ErrorIs(err, boom, "GetSkill with an unreadable store: got")
	assert.NewAborting(t).NotErrorIs(err, connectapi.ErrSkillNotFound, "a store failure must not read as NotFound")
}
