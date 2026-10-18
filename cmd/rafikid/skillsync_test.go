// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

func TestBuildNamespacesGroupsAndSortsDeterministically(t *testing.T) {
	c := assert.NewCollecting(t)
	got := buildNamespaces([]skills.Record{
		{Namespace: "rafiki", Name: "zebra", Description: "z", Body: "bz"},
		{Namespace: "pg", Name: "alpha", Description: "a", Body: "ba"},
		{Namespace: "rafiki", Name: "apple", Description: "ap", Body: "bap"},
	}, "1.2.3")

	c.Require().Len(got, 2, "got %d namespaces, want 2", len(got))
	// Sorted: the payload is compared for equality on the executor to decide
	// whether to rewrite, so an unstable order would rewrite every sync.
	if got[0].GetName() != "pg" || got[1].GetName() != "rafiki" {
		t.Fatalf("namespaces not sorted: %s, %s", got[0].GetName(), got[1].GetName())
	}
	rafiki := got[1]
	c.Require().Len(rafiki.GetSkills(), 2, "got %d skills in rafiki, want 2", len(rafiki.GetSkills()))
	c.Eq("apple", rafiki.GetSkills()[0].GetName(), "skills not sorted: first is")
	c.Eq("1.2.3", rafiki.GetVersion(), "version")
	// Names travel BARE: Claude Code derives "rafiki:apple" from the plugin
	// directory, so a prefix here would be applied twice.
	for _, s := range rafiki.GetSkills() {
		if len(s.GetName()) > 0 && s.GetName()[0] == 'r' && len(s.GetName()) > 7 && s.GetName()[:7] == "rafiki:" {
			t.Errorf("skill name %q carries a namespace prefix", s.GetName())
		}
	}
}

// An empty corpus is a full removal on the executor. That is a legitimate
// instruction and a catastrophic accident, so the pusher refuses to send one:
// a database blip that returned zero rows must not wipe every machine.
func TestPushRefusesToSendAnEmptyCorpus(t *testing.T) {
	c := assert.NewCollecting(t)
	c.False(shouldPush(nil), "pusher would send an empty corpus; a zero-row read must not wipe executors")
	c.True(shouldPush([]skills.Record{{Namespace: "rafiki", Name: "a", Body: "b"}}), "pusher refused a non-empty corpus")
}

func TestEligibleRequiresSkillsSyncAndClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	eligible := func(d *executorpb.DescribeResponse) bool {
		return (&skillPusher{}).eligible(execpool.LiveExecutor{Executor: executors.Executor{ID: "e1"}, Describe: d})
	}
	c.True(eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"claude"}}), "an executor that accepts syncs and launches claude children must be eligible")
	c.False(eligible(&executorpb.DescribeResponse{SkillsSync: false, LaunchKinds: []string{"claude"}}), "an executor that did not opt into syncs must not be eligible")
	c.False(eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"fundi"}}), "an executor that launches no claude children must not be eligible")
	// A nil Describe must be declined, not dereferenced — the connect hook can
	// fire against a pool entry before its Describe has landed.
	c.False(eligible(nil), "a nil Describe must not be eligible (and must not panic)")
}

// pushStore is a store holding one core row, enough for every verb to resolve.
func pushStore() *fakeSkillStore {
	return &fakeSkillStore{rows: []skills.Record{
		{ID: "id-1", Namespace: "rafiki", Name: "a", Source: skills.CoreSource},
	}}
}

// The ticker is gone: skillPusher has no Run, and the corpus is refreshed on
// write instead (connectSkills.push below) and on executor connect (main.go's
// SetOnConnect). These pin the adapter side of that contract — push fires after
// a successful write, never after a failed one, and a nil push is tolerated
// since existing wiring constructs connectSkills without one.
func TestConnectSkillsPushesOnDelete(t *testing.T) {
	ck := assert.NewAborting(t)
	called := false
	c := connectSkills{
		st:   pushStore(),
		push: func(context.Context) { called = true },
	}
	ck.NoError(c.DeleteSkill(context.Background(), "rafiki", "a"), "delete")
	ck.True(called, "DeleteSkill did not push after a successful write")
}

func TestConnectSkillsPushesOnSetEnabled(t *testing.T) {
	ck := assert.NewAborting(t)
	called := false
	c := connectSkills{
		st:   pushStore(),
		push: func(context.Context) { called = true },
	}
	ck.NoError(c.SetSkillEnabled(context.Background(), "rafiki", "a", false), "set enabled")
	ck.True(called, "SetSkillEnabled did not push after a successful write")
}

// A write that did not happen pushes nothing: the corpora the executors already
// hold still match the store.
func TestConnectSkillsPushesNothingOnAFailedWrite(t *testing.T) {
	boom := errors.New("db is gone")
	for _, tc := range []struct {
		name   string
		store  *fakeSkillStore
		invoke func(connectSkills) error
	}{
		{"delete failed", &fakeSkillStore{
			rows: pushStore().rows, deleteErr: boom,
		}, func(c connectSkills) error { return c.DeleteSkill(context.Background(), "rafiki", "a") }},
		{"disable failed", &fakeSkillStore{
			rows: pushStore().rows, enableErr: boom,
		}, func(c connectSkills) error { return c.SetSkillEnabled(context.Background(), "rafiki", "a", false) }},
		{"nothing to write", &fakeSkillStore{}, func(c connectSkills) error {
			return c.DeleteSkill(context.Background(), "rafiki", "absent")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c := connectSkills{st: tc.store, push: func(context.Context) { called = true }}
			assert.NewAborting(t).Error(tc.invoke(c), "write unexpectedly succeeded")
			assert.NewAborting(t).False(called, "push fired on a failed write")
		})
	}
}

func TestConnectSkillsToleratesNilPush(t *testing.T) {
	c := connectSkills{st: pushStore()}
	ck := assert.NewAborting(t)
	ck.NoError(c.DeleteSkill(context.Background(), "rafiki", "a"), "delete")
	ck.NoError(c.SetSkillEnabled(context.Background(), "rafiki", "a", true), "set enabled")
}
