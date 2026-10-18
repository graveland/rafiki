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
	body, err := ro.InlineSkillBody(ctx, ns, "before")
	ck.Require().NoError(err, "inline body")
	ck.Require().Eq("before body", body, "inline body")

	// A skill written after the spawn is not in the inventory, so the child was
	// never offered it and the body is refused.
	put(rec("before", "before body"), rec("after", "after body"))
	_, err = ro.InlineSkillBody(ctx, ns, "after")
	ck.ErrorIs(err, skills.ErrNotFound, "a skill absent from the spawn-time inventory: got")

	// So is a name that never existed.
	_, err = ro.InlineSkillBody(ctx, ns, "never")
	ck.ErrorIs(err, skills.ErrNotFound, "an unknown skill: got")
}

// fakeSkillStore records what the adapter asked for. List answers from rows;
// every other method records the id it was handed, which is the property these
// tests care about — a verb that passed a (namespace, name) would reach the
// store with a key that is only unique within a layer.
type fakeSkillStore struct {
	rows      []skills.Record
	listErr   error
	getErr    error
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
			return r, nil
		}
	}
	return skills.Record{}, skills.ErrNotFound
}

func (f *fakeSkillStore) Files(context.Context, []string) (map[string][]skills.File, error) {
	return nil, nil
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
// core row. Another user's row is not addressable from here at all.
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
			if err := c.DeleteSkill(tc.ctx, "rafiki", "s"); tc.wantErr != nil {
				assert.NewAborting(t).ErrorIs(err, tc.wantErr, "DeleteSkill: got")
				return
			} else if err != nil {
				t.Fatalf("DeleteSkill: %v", err)
			}
			assert.NewAborting(t).Eq(tc.wantID, f.deletedID, "DeleteSkill reached the store with id")
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
