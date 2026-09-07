// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/skills"
)

func TestBuildNamespacesGroupsAndSortsDeterministically(t *testing.T) {
	got := buildNamespaces([]skills.Record{
		{Namespace: "rafiki", Name: "zebra", Description: "z", Body: "bz"},
		{Namespace: "pg", Name: "alpha", Description: "a", Body: "ba"},
		{Namespace: "rafiki", Name: "apple", Description: "ap", Body: "bap"},
	}, "1.2.3")

	if len(got) != 2 {
		t.Fatalf("got %d namespaces, want 2", len(got))
	}
	// Sorted: the payload is compared for equality on the executor to decide
	// whether to rewrite, so an unstable order would rewrite every sync.
	if got[0].GetName() != "pg" || got[1].GetName() != "rafiki" {
		t.Fatalf("namespaces not sorted: %s, %s", got[0].GetName(), got[1].GetName())
	}
	rafiki := got[1]
	if len(rafiki.GetSkills()) != 2 {
		t.Fatalf("got %d skills in rafiki, want 2", len(rafiki.GetSkills()))
	}
	if rafiki.GetSkills()[0].GetName() != "apple" {
		t.Errorf("skills not sorted: first is %q", rafiki.GetSkills()[0].GetName())
	}
	if rafiki.GetVersion() != "1.2.3" {
		t.Errorf("version %q not stamped", rafiki.GetVersion())
	}
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
	if shouldPush(nil) {
		t.Error("pusher would send an empty corpus; a zero-row read must not wipe executors")
	}
	if !shouldPush([]skills.Record{{Namespace: "rafiki", Name: "a", Body: "b"}}) {
		t.Error("pusher refused a non-empty corpus")
	}
}

func TestEligibleRequiresSkillsSyncAndClaude(t *testing.T) {
	eligible := func(d *executorpb.DescribeResponse) bool {
		return (&skillPusher{}).eligible(execpool.LiveExecutor{Executor: executors.Executor{ID: "e1"}, Describe: d})
	}
	if !eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"claude"}}) {
		t.Error("an executor that accepts syncs and launches claude children must be eligible")
	}
	if eligible(&executorpb.DescribeResponse{SkillsSync: false, LaunchKinds: []string{"claude"}}) {
		t.Error("an executor that did not opt into syncs must not be eligible")
	}
	if eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"fundi"}}) {
		t.Error("an executor that launches no claude children must not be eligible")
	}
	// A nil Describe must be declined, not dereferenced — the connect hook can
	// fire against a pool entry before its Describe has landed.
	if eligible(nil) {
		t.Error("a nil Describe must not be eligible (and must not panic)")
	}
}
