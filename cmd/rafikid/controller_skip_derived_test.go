package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// skipFixture returns a Controller backed by an in-memory store, with an
// optional parent session registered under c_parent.
func skipFixture(t *testing.T, parent *childstore.Session) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	if parent != nil {
		c.st.Insert(parent)
	}
	return c
}

func mustSnapshot(t *testing.T, c *Controller, childID string) childstore.Snapshot {
	t.Helper()
	snap, ok := c.st.Get(childID)
	assert.NewAborting(t).Require().True(ok, "child %s not found in store", childID)
	return snap
}

// A child under a flagged parent inherits the flag even when it asks for the
// default: the OR is the whole point, and dropping it re-enables derived
// indexing for the subtree.
func TestInheritSkipDerivedIndexParentSetsChild(t *testing.T) {
	ck := assert.NewAborting(t)
	c := skipFixture(t, &childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindFundi, SkipDerivedIndex: true,
	})

	req := c.inheritSkipDerivedIndex(protocol.SpawnRequest{
		ParentChildID: "c_parent", SkipDerivedIndex: false,
	})
	ck.True(req.SkipDerivedIndex, "a child under a flagged parent must inherit the flag")
}

// The other half of the OR: a child may switch derived indexing off beneath a
// parent that left it on.
func TestInheritSkipDerivedIndexChildCanSetUnderUnflaggedParent(t *testing.T) {
	ck := assert.NewAborting(t)
	c := skipFixture(t, &childstore.Session{ChildID: "c_parent", Kind: protocol.KindFundi})

	req := c.inheritSkipDerivedIndex(protocol.SpawnRequest{
		ParentChildID: "c_parent", SkipDerivedIndex: true,
	})
	ck.True(req.SkipDerivedIndex, "a child may switch derived indexing off beneath an unflagged parent")
}

// A top-level spawn has no parent to inherit from; whatever it asked for stays.
func TestInheritSkipDerivedIndexTopLevelUntouched(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := skipFixture(t, nil)

	for _, want := range []bool{true, false} {
		req := c.inheritSkipDerivedIndex(protocol.SpawnRequest{SkipDerivedIndex: want})
		ck.Eq(want, req.SkipDerivedIndex, "a top-level spawn must keep its own value; want=%v", want)
	}
}

// An unknown parent id inherits nothing, in both directions.
func TestInheritSkipDerivedIndexUnknownParentInheritsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)

	for _, want := range []bool{true, false} {
		c := skipFixture(t, nil)
		req := c.inheritSkipDerivedIndex(protocol.SpawnRequest{
			ParentChildID: "c_absent", SkipDerivedIndex: want,
		})
		ck.Eq(want, req.SkipDerivedIndex, "an unknown parent must change nothing; want=%v", want)
	}
}

// A snapshot's flag must reach BOTH places resume rebuilds it into: the
// respawn request resumeRequestFromSnapshot returns, and the resumed Session
// activateLiveChild inserts. Dropping it from either silently re-enables
// derived indexing on the first resume.
func TestSkipDerivedIndexSurvivesResumeSnapshot(t *testing.T) {
	ck := assert.NewAborting(t)

	snap := childstore.Snapshot{
		Cwd: "/tmp", Kind: protocol.KindClaude, SessionID: "sess-1", SkipDerivedIndex: true,
	}
	req := resumeRequestFromSnapshot(snap, "")
	ck.True(req.SkipDerivedIndex, "the respawn request dropped SkipDerivedIndex")

	ctrl := newTestController(t)
	ctx := t.Context()

	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:             protocol.KindClaude,
		Cwd:              t.TempDir(),
		PiBinary:         fakePiBin(t),
		NoSession:        true,
		SkipDerivedIndex: true,
	}, users.Identity{})
	ck.Require().NoError(err, "spawn")
	ck.True(mustSnapshot(t, ctrl, res.ChildID).SkipDerivedIndex,
		"the spawn Session literal dropped SkipDerivedIndex")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, res.ChildID, 2000, 500)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)

	_, err = ctrl.Resume(ctx, res.ChildID, "")
	ck.Require().NoError(err, "resume")
	ck.True(mustSnapshot(t, ctrl, res.ChildID).SkipDerivedIndex,
		"the resumed Session literal dropped SkipDerivedIndex")
}

// The Connect plane must carry the flag onto the framed request Controller.Spawn
// applies; an unmapped field is a silent drop on the cockpit's spawn path.
func TestBuildProtocolSpawnRequestCarriesSkipDerivedIndex(t *testing.T) {
	got := buildProtocolSpawnRequest(connectapi.SpawnParams{
		Cwd: "/work", Kind: "fundi", SkipDerivedIndex: true,
	})
	assert.NewAborting(t).True(got.SkipDerivedIndex, "buildProtocolSpawnRequest dropped SkipDerivedIndex")
}
