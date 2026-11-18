// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

var executorSetShapes = map[string][]string{
	"legacy nil": nil,
	"empty":      {},
	"all":        {childstore.ExecutorSetAll},
	"ids":        {"x_1", "x_2"},
}

// A resumed child must keep the set it was spawned with: the request rebuilt
// from the snapshot carries it, nil and empty staying distinct.
func TestResumeRequestFromSnapshotCarriesExecutorSet(t *testing.T) {
	for name, set := range executorSetShapes {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			req := resumeRequestFromSnapshot(childstore.Snapshot{
				Cwd: "/tmp", Kind: protocol.KindClaude, SessionID: "s", ExecutorSet: set,
			}, "")
			ck.EqDeep(set, req.Executors, "set")
			ck.Eq(set == nil, req.Executors == nil, "nil-ness changed")
		})
	}
}

// The Session activateLiveChild rebuilds from the snapshot must carry the set
// too, or the first resume rewrites the stored row to a legacy nil.
func TestActivateLiveChildKeepsExecutorSet(t *testing.T) {
	for name, set := range executorSetShapes {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ctrl := newTestController(t)
			res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
				Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t), NoSession: true,
			}, users.Identity{})
			ck.Require().NoError(err, "spawn")
			ck.Require().NoError(ctrl.st.SetExecutorSet(res.ChildID, set), "set")

			killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
			ck.Require().NoError(err, "kill")
			waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)
			_, err = ctrl.Resume(t.Context(), res.ChildID, "")
			if all, _, _ := childstore.ResolveSetSpec(set, ""); !all {
				// With no executor pool a confined claude child cannot be hosted:
				// the local-fork guard refuses the resume, and the refusal must
				// leave the stored set exactly as it was.
				ck.Require().Error(err, "a confined claude child must not resume as a local fork")
				ck.StrContains(err.Error(), "confined to the executor plane", "refusal text")
			} else {
				ck.Require().NoError(err, "resume")
			}

			got := mustSnapshot(t, ctrl, res.ChildID).ExecutorSet
			ck.EqDeep(set, got, "set")
			ck.Eq(set == nil, got == nil, "nil-ness changed")
		})
	}
}

// A fundi child is hosted in-process, so no local-fork guard ever refuses its
// resume: this is the path that reaches activateLiveChild with a finite or empty
// set, which the claude case above never does. An activation that collapsed an
// empty set to nil, or rewrote ids to all, fails here.
func TestActivateLiveChildKeepsExecutorSetOnAnUnrefusedResume(t *testing.T) {
	for name, set := range executorSetShapes {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ctrl := newTestController(t)
			childID := "c_fundi_" + name
			seedExitedFundiChild(t, ctrl, childID, "", nil)
			if set != nil {
				ck.Require().NoError(ctrl.st.SetExecutorSet(childID, set), "set")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err := ctrl.Resume(ctx, childID, "")
			ck.Require().NoError(err, "a fundi resume is hosted in-process and is never refused for its set")

			got := mustSnapshot(t, ctrl, childID).ExecutorSet
			ck.EqDeep(set, got, "set")
			ck.Eq(set == nil, got == nil, "nil-ness changed")
		})
	}
}
