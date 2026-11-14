package main

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// probeBinder scripts a ProvisionOn failure per executor on top of fakeBinder,
// so a home probe can be driven to "this machine cannot answer" — the state a
// real executor reports as CodeInvalidArgument when the child's cwd does not
// exist in its filesystem.
type probeBinder struct {
	*fakeBinder
	provErr map[string]error
}

func (p *probeBinder) ProvisionOn(ctx context.Context, execID string, useCwd bool) (string, string, tools.ExecutorClient, error) {
	if err, ok := p.provErr[execID]; ok {
		return "", "", nil, err
	}
	return p.fakeBinder.ProvisionOn(ctx, execID, useCwd)
}

// homeFixture is a controller plus a fake binder over ids, and the reachable
// set the binder reports right now.
func homeFixture(t *testing.T, ids ...string) (*Controller, *fakeBinder, *boundExecutor, []executors.Executor) {
	t.Helper()
	c := newTestController(t)
	execs := make([]*fakeExec, 0, len(ids))
	for _, id := range ids {
		execs = append(execs, &fakeExec{id: id})
	}
	fb := newFakeBinder(execs...)
	be := newBoundExecutor("c1", fb)
	reach, err := fb.Reachable("c1")
	assert.NewAborting(t).Require().NoError(err, "Reachable")
	return c, fb, be, reach
}

// missingWorkdir is what an executor's Provision returns for a cwd it cannot
// see (connect.CodeInvalidArgument from pkg/executor's resolveWorkdir).
func missingWorkdir(cwd string) error {
	return connect.NewError(connect.CodeInvalidArgument,
		errors.New(`workdir "`+cwd+`" does not exist in this executor's filesystem`))
}

// An explicit home_executor outside this spawn's reachable set REFUSES the
// spawn: field 35 being child-allowed rests on that refusal, so anything else
// (a quietly ignored home, or a fallback to another machine) would let a caller
// think it chose where its project instructions come from when it did not.
func TestHomeExecutorExplicitOutsideSetRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")

	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{HomeExecutor: "nowhere"}, be, reach)
	ck.Require().Error(err, "a home executor outside the reachable set must refuse the spawn")
	ck.EqualError(err, `home executor "nowhere" is not in this spawn's reachable executors (a, b)`)
	ck.Eq("", home, "homeID")
	ck.Len(warnings, 0, "a refused spawn reports no warning")
	ck.Eq(0, fb.provisions, "nothing may be provisioned before the home is decided")
}

// A named home in the reachable set is the answer, with no probe and no
// provisioning: the caller already said where its project tree lives.
func TestHomeExecutorExplicitInSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")

	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{HomeExecutor: "b"}, be, reach)
	ck.Require().NoError(err)
	ck.Eq("b", home, "homeID")
	ck.Len(warnings, 0, "a named, reachable home needs no warning")
	ck.Eq(0, fb.provisions, "a named home must not be probed")
}

// ExecutorRef is a home candidate: a spawn pinned to one machine takes its
// project tier from that machine.
func TestHomeExecutorFromRef(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")

	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{ExecutorRef: "b"}, be, reach)
	ck.Require().NoError(err)
	ck.Eq("b", home, "homeID")
	ck.Len(warnings, 0, "warnings")
	ck.Eq(0, fb.provisions, "no probe when the ref names the home")
}

// A descendant reads its project tier where its spawner's lives: the parent's
// recorded home, when reachable, is the child's home.
func TestHomeExecutorInheritedFromParent(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")
	fb.homeLabel = "b" // the parent's row

	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{ParentChildID: "c_parent"}, be, reach)
	ck.Require().NoError(err)
	ck.Eq("b", home, "the child must inherit the home its parent's project tree lives on")
	ck.Len(warnings, 0, "warnings")
	ck.Eq(0, fb.provisions, "inheritance needs no probe")
}

// An inherited home OUTSIDE reach is skipped, never used: reach is the
// confinement-narrowed live set, and home can only ever be a member of it.
func TestHomeExecutorParentHomeOutsideSetSkipped(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, _ := homeFixture(t, "a", "b")
	fb.homeLabel = "b"
	fb.reachableTo("a")
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")

	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{ParentChildID: "c_parent"}, be, reach)
	ck.Require().NoError(err)
	ck.Eq("a", home, "the parent's unreachable home must be skipped, and the single reachable executor is home")
	ck.Len(warnings, 0, "warnings")
	ck.Eq(0, fb.provisionsOn("b"), "the skipped candidate must never be provisioned")
}

// One reachable executor needs no probing at all: there is nothing to choose.
func TestHomeExecutorSingleReachable(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, _ := homeFixture(t, "a", "b")
	fb.reachableTo("a")
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err)
	ck.Eq("a", home, "homeID")
	ck.Len(warnings, 0, "warnings")
	ck.Eq(0, fb.provisions, "a single reachable executor is home without a probe")
}

// Exactly one machine has the child's cwd: it is home, its probe workspace
// IS the home binding, and the spawn-time eager bind must not provision a
// second one on top of it.
func TestHomeExecutorProbeOneHitAdoptsBinding(t *testing.T) {
	ck := assert.NewAborting(t)
	execs := []*fakeExec{{id: "a"}, {id: "b"}, {id: "c"}}
	fb := newFakeBinder(execs...)
	pb := &probeBinder{fakeBinder: fb, provErr: map[string]error{
		"b": missingWorkdir("/tmp"),
		"c": missingWorkdir("/tmp"),
	}}
	be := newBoundExecutor("c1", pb)
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	c := newTestController(t)

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("a", home, "the one machine holding the cwd is home")
	ck.Len(warnings, 0, "one hit needs no warning")

	// The probe's workspace became the binding: the eager bind the spawn path
	// does next finds it and provisions NOTHING.
	_, eagerErr := be.clientFor(context.Background(), home)
	ck.Require().NoError(eagerErr, "the eager bind must reuse the probe's workspace")
	ck.Eq(1, fb.provisionsOn("a"), "the probe's workspace must be adopted, not provisioned twice")
	execID, wsID, bound := be.Current()
	ck.Eq("a", execID, "home")
	ck.True(bound && wsID != "", "the adopted workspace must be the home binding, found by teardown's labels (ws=%q)", wsID)
	ck.Eq(0, len(fb.released), "nothing is released when exactly one machine wins")
}

// Several machines hold the child's cwd: the daemon has no honest way to pick
// one, so none becomes home, every probe workspace is released, and the
// operator is asked to name one.
func TestHomeExecutorProbeSeveralHitsWarnsAndReleases(t *testing.T) {
	ck := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"}, &fakeExec{id: "c"})
	be := newBoundExecutor("c1", fb)
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	c := newTestController(t)

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("", home, "an ambiguous cwd must leave the child with NO home")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.Eq("cwd /tmp exists on a, b, c; pass home_executor to choose where project instructions come from",
		warnings[0], "warning text")
	ck.Eq(3, fb.provisions, "every candidate was probed")
	ck.Eq(3, len(fb.released), "every probe workspace must be released: %v", fb.releasedWorkspaces())
	_, _, bound := be.Current()
	ck.False(bound, "an ambiguous probe must not leave a binding behind")
}

// Every machine refused the child's cwd: this is the refusal an eager bind has
// always produced, reused rather than reinvented.
func TestHomeExecutorProbeNoneRefusesMissingCwd(t *testing.T) {
	ck := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	pb := &probeBinder{fakeBinder: fb, provErr: map[string]error{
		"a": missingWorkdir("/tmp"),
		"b": missingWorkdir("/tmp"),
	}}
	be := newBoundExecutor("c1", pb)
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	c := newTestController(t)

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().Error(err, "a cwd no machine holds must refuse the spawn")
	ck.StrContains(err.Error(), "workspace on executor a:", "the refusal reuses the eager bind's wrap: %v", err)
	ck.StrContains(err.Error(), `workdir "/tmp" does not exist`, "the refusal carries the executor's own message: %v", err)
	ck.Eq("", home, "homeID")
	ck.Len(warnings, 0, "a refusal reports no warning")
}

// No rule names a home and there is nothing to probe (no cwd to look for): the
// child starts unbound with an explicit warning, rather than guessing a machine
// its project instructions did not come from.
func TestHomeExecutorNoHomeWarns(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{}, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("", home, "homeID")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.Eq(noHomeWarning, warnings[0], "warning text")
	ck.Eq(0, fb.provisions, "an empty cwd is nothing to probe")
}

// A child whose home resolved but whose machine is gone gets NO project context
// and is told why — never another machine's instructions.
func TestHomeExecutorOfflineHomeWarnsNoContext(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, _ := homeFixture(t, "a", "b")
	fb.reachableTo("a") // b has departed
	be.setHome("b")
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, "b", true, reach)
	ck.Eq("", info.Project, "an unreachable home must not yield project instructions")
	ck.Require().Len(info.Warnings, 1, "warnings = %v", info.Warnings)
	ck.Eq("home executor b is not connected; project instructions were not loaded",
		info.Warnings[0], "warning text")
	ck.Eq(0, fb.provisionsOn("b"), "the offline home must not be provisioned for a fetch")
	ck.Nil(info.Workspace, "no machine block for a machine with no live row")
}

// With two reachable executors, the project tier comes from the HOME and only
// the home: reading another machine's CLAUDE.md would hand the model
// instructions written for a tree it cannot see.
func TestHomeExecutorProjectContextFromHomeOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	c, fb, be, reach := homeFixture(t, "a", "b")
	home, warnings, err := c.resolveHomeExecutor(context.Background(),
		protocol.SpawnRequest{HomeExecutor: "b", Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Len(warnings, 0, "warnings")
	ck.Eq("b", home, "homeID")
	be.setHome(home)

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, home, true, reach)
	ck.Eq("b:ctx", info.Project, "project context must come from the home executor")
	ck.Eq(1, fb.provisionsOn("b"), "the home was read once")
	ck.Eq(0, fb.provisionsOn("a"), "the non-home executor must never be read for project context")
}

// A resumed child comes back to the home recorded on its row, never to a
// different reachable machine — and when that machine is offline it has NO home
// plus the not-connected warning, rather than becoming unresumable.
func TestHomeExecutorResumeKeepsPersistedHome(t *testing.T) {
	ck := assert.NewAborting(t)
	snap := childstore.Snapshot{
		ChildID: "c1",
		Kind:    protocol.KindFundi,
		Labels:  map[string]string{homeExecutorLabel: "a"},
	}
	req := resumeRequestFromSnapshot(snap, "")
	ck.Eq("a", req.HomeExecutor, "the resume request must carry the child's persisted home")

	c, fb, be, reach := homeFixture(t, "a", "b")
	fb.homeLabel = "a" // the child's own row, which is what Home reads

	home, warnings, err := c.resolveHomeExecutor(context.Background(), req, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("a", home, "a resumed child keeps the home it had, never a different reachable machine")
	ck.Len(warnings, 0, "warnings")

	fb.reachableTo("b")
	reach, err = fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	home, warnings, err = c.resolveHomeExecutor(context.Background(), req, be, reach)
	ck.Require().NoError(err, "an offline persisted home must not make the child unresumable")
	ck.Eq("", home, "a persisted home outside reach yields NO home, never a different executor")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.Eq("home executor a is not connected; project instructions were not loaded",
		warnings[0], "warning text")
	ck.Eq(0, fb.provisionsOn("b"), "the child must not be moved onto the machine that is there")
}

// Field 35 is child-allowed on the strength of rule 1's refusal, so it must
// refuse at the CHILD-provenance boundary too: a child caller naming a home
// outside its reachable set gets the refusal, not a silently-ignored home that
// leaves it reading somebody else's project tree.
func TestHomeExecutorChildSpawnOutsideSetRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{live: []execpool.LiveExecutor{
		ex("exec-1", map[string]string{"machine": "one"}, ""),
		ex("exec-2", map[string]string{"machine": "two"}, ""),
	}}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	parentID := seedChild(t, c)

	req := baseRequest()
	req.ParentChildID = parentID // the shape a child-credential spawn arrives in
	req.HomeExecutor = "nowhere"
	var warnings []string
	_, err := c.agentRuntimeOptionsFor(req, "c1", false, "brent", "u-1", "u-1", "", &warnings)
	ck.Require().Error(err, "a child spawn naming a home outside its reachable set must be refused")
	ck.StrContains(err.Error(), `home executor "nowhere" is not in this spawn's reachable executors`,
		"the child gets the rule-1 refusal, not a silent fallback: %v", err)
	ck.Len(warnings, 0, "a refused spawn reports no warning")
}

// The home recorded at spawn must survive the other half of the pre-spawn
// stash. A successful eager bind calls controllerBinder.NoteBinding, which
// REPLACES this child's stash struct, so the home is recorded after it — the
// reverse order silently loses the home, and Spawn then writes no
// rafiki/home-executor label, so a resume re-resolves rules 2–5 and can move
// the child off the machine its cwd came from.
func TestHomeExecutorLabelSurvivesTheSpawnStash(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)

	(&controllerBinder{c: c}).NoteBinding("c_new", "exec-a", "ws-1") // the eager bind
	c.noteHomeExecutor("c_new", "exec-a")                            // then the home

	wl, ok := c.takeWorkspaceLabels("c_new")
	ck.True(ok, "Spawn consumes this stash exactly once")
	ck.Eq("exec-a", wl.homeExecutorID, "the home must survive the binding stash")
	ck.Eq("exec-a", wl.executorID, "the binding's executor survives too")
	ck.Eq("ws-1", wl.workspaceID, "the binding's workspace survives too")
}

// A home the child's row already records is never rewritten to a different id:
// a resolver that changed its mind must not move a running child's home on
// disk behind its back.
func TestHomeExecutorNoteNeverRewritesADifferentHome(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	childID := seedChild(t, c)
	_, setErr := c.st.SetLabels(childID, map[string]string{homeExecutorLabel: "a"}, nil)
	ck.Require().NoError(setErr, "seed the persisted home")

	c.noteHomeExecutor(childID, "b")
	snap, ok := c.st.Get(childID)
	ck.True(ok, "child vanished")
	ck.Eq("a", snap.Labels[homeExecutorLabel], "the persisted home must stand")

	c.noteHomeExecutor(childID, "a") // the same home is fine, and idempotent
	snap, _ = c.st.Get(childID)
	ck.Eq("a", snap.Labels[homeExecutorLabel], "writing the same home changes nothing")
}
