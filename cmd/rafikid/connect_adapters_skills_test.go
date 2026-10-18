// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

// The skills surface is a permission boundary as much as a view: a shared row
// (core and global) belongs to the daemon, and only an admin or the local
// socket may change it. A user's own rows are theirs. These tests pin each
// refusal and its route forward.

// skillUserCtx is a real user credential, the only provenance the policy
// interceptor admits to these verbs besides the socket.
func skillUserCtx(userID string, admin bool) context.Context {
	return server.WithIdentity(context.Background(), &server.Identity{
		UserID: userID, Username: userID, Via: server.ProvenanceUser, IsAdmin: admin,
	})
}

func TestSkillSetEnabledOwnRowAllowed(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "s", Source: skills.CoreSource},
		{ID: "own-id", Namespace: "rafiki", Name: "s", Source: "manual", OwnerUserID: "u1"},
	}}
	c := connectSkills{st: f}

	ck.NoError(c.SetSkillEnabled(skillUserCtx("u1", false), "rafiki", "s", false), "a user may toggle their own row")
	ck.Eq("own-id", f.enabledID, "SetSkillEnabled reached the store with id")
	ck.False(f.enabledTo, "SetSkillEnabled sent enabled=true")
}

func TestSkillSetEnabledSharedRowRefusedForNonAdmin(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "global-id", Namespace: "rafiki", Name: "s", Source: "manual"},
	}}
	c := connectSkills{st: f}

	err := c.SetSkillEnabled(skillUserCtx("u1", false), "rafiki", "s", false)
	ck.Require().Error(err, "a non-admin toggled a shared row")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "refusal code for a shared row: got %v", err)
	ck.StrContains(err.Error(),
		"shared skill rafiki:s can only be changed by an admin or over the local socket",
		"refusal message")
	ck.Eq("", f.enabledID, "the store was reached despite the refusal")
}

func TestSkillSetEnabledSharedRowAllowedForAdmin(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "global-id", Namespace: "rafiki", Name: "s", Source: "manual"},
	}}
	c := connectSkills{st: f}

	ck.NoError(c.SetSkillEnabled(skillUserCtx("u1", true), "rafiki", "s", false), "an admin may toggle a shared row")
	ck.Eq("global-id", f.enabledID, "SetSkillEnabled reached the store with id")
}

func TestSkillSetEnabledSharedRowAllowedOnSocket(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "s", Source: skills.CoreSource},
	}}
	c := connectSkills{st: f}

	// A nil identity is the unix socket: local trust, no user, no credential.
	ck.NoError(c.SetSkillEnabled(context.Background(), "rafiki", "s", false), "the local socket may toggle a shared row")
	ck.Eq("core-id", f.enabledID, "SetSkillEnabled reached the store with id")
}

func TestSkillDeleteRefusesCore(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "s", Source: skills.CoreSource},
	}}
	c := connectSkills{st: f}

	// The socket is allowed to write a shared row, so this refusal is about the
	// SOURCE, not the caller: a core row comes back on the next sync.
	err := c.DeleteSkill(context.Background(), "rafiki", "s")
	ck.Require().Error(err, "a core skill was deleted")
	ck.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "refusal code for a core row: got %v", err)
	ck.StrContains(err.Error(),
		`rafiki:s is a core skill; disable it with "rafiki skills disable rafiki:s"`,
		"refusal message")
	ck.Eq("", f.deletedID, "the store was reached despite the refusal")
}

func TestSkillDeleteRefusesRepo(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "repo-id", Namespace: "pg", Name: "tables", Source: "repo:mine", OwnerUserID: "u1"},
	}}
	c := connectSkills{st: f}

	err := c.DeleteSkill(skillUserCtx("u1", false), "pg", "tables")
	ck.Require().Error(err, "a repo-owned skill was deleted")
	ck.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "refusal code for a repo row: got %v", err)
	ck.StrContains(err.Error(),
		`pg:tables comes from repo "mine"; remove the repo with "rafiki repo remove mine"`,
		"refusal message")
	ck.Eq("", f.deletedID, "the store was reached despite the refusal")
}

func TestSkillDeleteLegacyManual(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "manual-id", Namespace: "rafiki", Name: "s", Source: "manual", OwnerUserID: "u1"},
	}}
	c := connectSkills{st: f}

	ck.NoError(c.DeleteSkill(skillUserCtx("u1", false), "rafiki", "s"), "a hand-authored row is the user's to delete")
	ck.Eq("manual-id", f.deletedID, "DeleteSkill reached the store with id")
}

func TestSkillListHidesOtherOwners(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "core", Source: skills.CoreSource, Enabled: true},
		{ID: "global-id", Namespace: "pg", Name: "global", Source: "manual", Enabled: true},
		{ID: "own-id", Namespace: "rafiki", Name: "mine", Source: "manual", OwnerUserID: "u1", Enabled: true},
		{ID: "other-id", Namespace: "rafiki", Name: "theirs", Source: "manual", OwnerUserID: "u2", Enabled: true},
	}}
	c := connectSkills{st: f}

	names := func(ctx context.Context) []string {
		t.Helper()
		rows, err := c.ListSkills(ctx, false)
		ck.Require().NoError(err, "ListSkills")
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	// A user sees the shared layers and their own row, never another owner's.
	ck.EqDeep([]string{"core-id", "global-id", "own-id"}, names(skillUserCtx("u1", false)),
		"u1's inventory")
	// The socket is nobody: only the shared layers.
	ck.EqDeep([]string{"core-id", "global-id"}, names(context.Background()),
		"the socket's inventory")
}

func TestSkillListMarksShadowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "x", Source: skills.CoreSource, Enabled: true},
		{ID: "own-id", Namespace: "rafiki", Name: "x", Source: "manual", OwnerUserID: "u1", Enabled: true},
		{ID: "unshadowed-id", Namespace: "rafiki", Name: "y", Source: skills.CoreSource, Enabled: true},
	}}
	c := connectSkills{st: f}

	rows, err := c.ListSkills(skillUserCtx("u1", false), false)
	ck.Require().NoError(err, "ListSkills")
	byID := make(map[string]int, len(rows))
	for i, r := range rows {
		byID[r.ID] = i
	}

	ck.Require().Len(rows, 3, "an inventory lists every visible row, shadowed ones included")
	core := rows[byID["core-id"]]
	ck.True(core.Shadowed, "the core row a user override replaces is listed as shadowed")
	ck.Eq("core", core.Layer, "core row layer")
	own := rows[byID["own-id"]]
	ck.False(own.Shadowed, "the overriding row is in effect")
	ck.Eq("user", own.Layer, "user row layer")
	ck.False(rows[byID["unshadowed-id"]].Shadowed, "a row nothing overrides is in effect")
}

func TestSkillTargetPrefersUserLayer(t *testing.T) {
	ck := assert.NewAborting(t)
	f := &fakeSkillStore{rows: []skills.Record{
		{ID: "core-id", Namespace: "rafiki", Name: "s", Source: skills.CoreSource},
		{ID: "global-id", Namespace: "rafiki", Name: "s", Source: "manual"},
		{ID: "own-id", Namespace: "rafiki", Name: "s", Source: "manual", OwnerUserID: "u1"},
	}}
	c := connectSkills{st: f}
	ctx := skillUserCtx("u1", false)

	// user > global > core: the verb acts on the row this caller would see.
	ck.NoError(c.SetSkillEnabled(ctx, "rafiki", "s", true), "SetSkillEnabled")
	ck.Eq("own-id", f.enabledID, "the user row wins")

	// Without a user row the global row is next, and only then core. The
	// global row is shared, so this step is an admin's — the target choice is
	// what is under test, not the permission.
	f.rows = f.rows[:2]
	ck.NoError(c.SetSkillEnabled(skillUserCtx("u1", true), "rafiki", "s", true), "SetSkillEnabled")
	ck.Eq("global-id", f.enabledID, "the global row wins over core")
}
