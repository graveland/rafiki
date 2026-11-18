package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

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

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, "b", true, nil, reach)
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

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, home, true, nil, reach)
	ck.Eq("b:ctx", info.Project, "project context must come from the home executor")
	ck.Eq(1, fb.provisionsOn("b"), "the home was read once")
	ck.Eq(0, fb.provisionsOn("a"), "the non-home executor must never be read for project context")
}

// A resumed child comes back to the home recorded on its row, never to a
// different reachable machine — and when that machine is offline it has NO home
// plus the not-connected warning, no machine block naming the executor that IS
// there, and it stays resumable.
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
	// The child's own row is what tells the resolver this home is PERSISTED
	// rather than a caller's declaration, so it must exist.
	c.st.Insert(&childstore.Session{ChildID: "c1", Status: protocol.StatusExited, Labels: snap.Labels})

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

	// And the prompt names no machine: the sole executor that IS reachable is
	// not where this child's cwd and CLAUDE.md came from, so a "Your machine"
	// block for it would be a lie the child repeats for its whole life. The
	// executor is LIVE (a real pool row), because that is what makes the
	// suppressed fallback able to name it at all. The resolver already warned,
	// so homeInfo adds nothing.
	c.execPool = &fakePool{live: []execpool.LiveExecutor{ex("b", map[string]string{"machine": "b"}, "")}}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	info := c.homeInfo(context.Background(), req, executorOwner{}, be, home, false, nil, reach)
	ck.Nil(info.Workspace, "no machine block may name the one executor that is still there")
	ck.Eq("", info.Project, "and no project tier may be read from it")
	ck.Len(info.Warnings, 0, "no second warning: the resolver's already said why")
}

// A child with no rafiki/home-executor label — everything spawned before it
// existed, or a child that started unbound and bound lazily — must still come
// back to its recorded rafiki/executor on resume. Resolving rules 2–5 instead
// would let it land on another reachable machine, whose cwd and CLAUDE.md are
// not the ones its conversation reads.
func TestHomeExecutorResumeFallsBackToTheBindingLabel(t *testing.T) {
	ck := assert.NewAborting(t)
	snap := childstore.Snapshot{
		ChildID: "c1",
		Kind:    protocol.KindFundi,
		Labels:  map[string]string{"rafiki/executor": "b"}, // no home label at all
	}
	req := resumeRequestFromSnapshot(snap, "")
	ck.Eq("b", req.HomeExecutor, "the resume request must fall back to rafiki/executor")

	c, fb, be, reach := homeFixture(t, "a", "b")
	c.st.Insert(&childstore.Session{ChildID: "c1", Status: protocol.StatusExited, Labels: snap.Labels})
	ck.Require().Len(reach, 2, "b must not be the only reachable executor, or rules 4/5 would agree by accident")

	home, warnings, err := c.resolveHomeExecutor(context.Background(), req, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("b", home, "the child's recorded binding executor is its home, not the other reachable machine")
	ck.Len(warnings, 0, "warnings")
	ck.Eq(0, fb.provisions, "no probe: the record already answered")
}

// A resume's home warning must reach the runtime's warning sink, not be
// discarded: Resume and RespawnChild pass the same collector Spawn does and log
// each entry at WARN, because neither has a caller response to carry it. Without
// that, a resumed child whose persisted home was offline came back without its
// CLAUDE.md and nothing said so anywhere.
func TestResumeRuntimeCollectsTheOfflineHomeWarning(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	c.st.Insert(&childstore.Session{
		ChildID:  "c1",
		Status:   protocol.StatusExited,
		Kind:     protocol.KindFundi,
		Cwd:      "/tmp",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-5",
		Labels:   map[string]string{homeExecutorLabel: "gone"},
	})
	snap, ok := c.st.Get("c1")
	ck.Require().True(ok, "seeded child")
	req := resumeRequestFromSnapshot(snap, "")
	ck.Eq("gone", req.HomeExecutor, "the resume request must carry the persisted home")

	var warnings []string
	_, err := c.agentRuntimeOptionsFor(req, "c1", true, "brent", "", "", "", &warnings)
	ck.NoError(err, "an offline persisted home must still build a runtime")
	ck.Require().NotEmpty(warnings, "the offline-home diagnostic must reach the sink, not be dropped")
	ck.StrContains(warnings[0],
		"home executor gone is not connected; project instructions were not loaded")
}

// Field 35 is child-allowed on the strength of rule 1's refusal, so the refusal
// must survive the CHILD-CREDENTIAL boundary: this spawns as a
// server.ProvenanceChildToken caller through the same connectLifecycle adapter
// the Control plane mounts, so a lost field mapping or a provenance-dependent
// refusal would fail here. A child naming a home outside its reachable set gets
// the refusal, never a silently-ignored home that leaves it reading somebody
// else's project tree.
func TestHomeExecutorChildSpawnOutsideSetRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{live: []execpool.LiveExecutor{
		ex("exec-1", map[string]string{"machine": "one"}, ""),
		ex("exec-2", map[string]string{"machine": "two"}, ""),
	}}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	parent := seedChild(t, c)
	_ = c.st.Update(parent, func(s *childstore.Session) {
		s.Kind = protocol.KindFundi
		s.MaxChildren = 8
		s.MaxDepth = 3
	})

	// The child-credential identity the rest of this package's tests use for a
	// per-child secret, acting as the parent child.
	ctx := childTokenCtx(parent)
	_, err := connectLifecycle{c: c}.Spawn(ctx, connectapi.SpawnParams{
		Cwd:           t.TempDir(),
		Kind:          protocol.KindFundi,
		Model:         "anthropic/claude-x",
		NoSession:     true,
		ParentChildID: parent,
		HomeExecutor:  "nowhere",
	})
	ck.Require().Error(err, "a child caller naming a home outside its reachable set must be refused")
	ck.StrContains(err.Error(), `home executor "nowhere" is not in this spawn's reachable executors`,
		"the child gets the rule-1 refusal, not a silent fallback: %v", err)
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

// A sandbox is never a probe candidate. It provisions at its own container
// workdir rather than the requested cwd, so it cannot answer "does this cwd
// exist here" — and it always succeeds, which would turn a clean single hit on a
// native executor into a false ambiguity and deny the child a home.
func TestHomeExecutorProbeSkipsASandbox(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	native := &fakeExec{id: "native"}
	box := &fakeExec{id: "sbx"}
	fb := newFakeBinder(native, box)
	be := newBoundExecutor("c1", fb)
	// A selector that admits a sandbox plus one native, both able to answer:
	// without the skip this is two hits and no home.
	reach := []executors.Executor{
		{ID: "sbx", Enabled: true, Labels: map[string]string{"machine": "box", sandbox.RowLabelSandbox: "1"}},
		{ID: "native", Enabled: true, Labels: map[string]string{"machine": "native"}},
	}

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "resolveHomeExecutor")
	ck.Eq("native", home, "the native executor holding the cwd is home")
	ck.Len(warnings, 0, "one hit needs no warning")
	ck.Eq(0, fb.provisionsOn("sbx"), "a sandbox must never be probed")
	ck.Eq(1, fb.provisionsOn("native"), "the native executor is probed once")
}

// Machines that cannot ANSWER are not evidence the child's cwd is wrong: a
// failure that is not a refused workdir yields the "could not check" warning
// naming each failure, and never a refusal or a guessed home.
func TestHomeExecutorProbeMixedFailuresWarn(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", &probeBinder{fakeBinder: fb, provErr: map[string]error{
		"a": connect.NewError(connect.CodeUnavailable, errors.New("executor link reset")),
		"b": missingWorkdir("/tmp"),
	}})
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")

	home, warnings, err := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "a machine that could not answer must not refuse the spawn")
	ck.Eq("", home, "no home from machines that could not answer")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.StrContains(warnings[0], "could not check cwd on:", "warning: %q", warnings[0])
	ck.StrContains(warnings[0], "a: ", "the warning must name every failing machine: %q", warnings[0])
	ck.StrContains(warnings[0], "b: ", "the warning must name every failing machine: %q", warnings[0])
	ck.StrContains(warnings[0], "executor link reset", "the real cause must survive: %q", warnings[0])
	ck.Require().Len(fb.released, 0, "neither probe made a workspace, so there is nothing to release")
}

// A probe whose budget is already spent cannot claim the cwd exists anywhere: it
// degrades to the "could not check" warning with no home — never a refusal, and
// never an adoption of a workspace whose id never arrived.
//
// NOTE on a REAL expired budget: the executor mints and registers the workspace
// BEFORE it answers, and pkg/executor has no reaper (Release is the only end of
// a workspace's retention — see Server.Release's comment). A Provision whose
// response is lost to the deadline therefore leaves one entry in the executor's
// in-memory registry, with its job set, until that executor process exits. It
// consumes no files of its own (the workdir is the child's cwd or the root), and
// the daemon holds no reference to it, so it is an orphan of bounded size per
// abandoned probe, not a disk leak. This test pins the DAEMON's half: no home, a
// warning, and nothing adopted.
func TestHomeExecutorProbeExpiredBudgetWarns(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)
	reach, err := fb.Reachable("c1")
	ck.Require().NoError(err, "Reachable")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the budget is spent before the first candidate is asked
	home, warnings, err := c.resolveHomeExecutor(ctx, protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
	ck.Require().NoError(err, "a spent budget must not refuse the spawn")
	ck.Eq("", home, "a spent budget names no home")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.StrContains(warnings[0], "could not check cwd on:", "warning: %q", warnings[0])
	ck.StrContains(warnings[0], "context canceled", "warning: %q", warnings[0])
	ck.Eq(0, fb.provisions, "a cancelled probe never made a workspace")
	if execID, _, bound := be.Current(); bound {
		t.Fatalf("nothing may be adopted on a spent budget; home binding is %q", execID)
	}
}

// A cwd the HOME cannot see REFUSES the spawn, with the SAME text the
// multi-executor probe gives a cwd no machine holds. It must not start unbound:
// that path does not heal, and every workspace call would then fail on a
// directory the child's prompt names as its own.
func TestHomeExecutorBadCwdOnTheHomeRefuses(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)

	// The probe's refusal for the same underlying error (a cwd no machine sees).
	probeFB := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	probeBE := newBoundExecutor("c1", &probeBinder{fakeBinder: probeFB, provErr: map[string]error{
		"a": missingWorkdir("/tmp"),
		"b": missingWorkdir("/tmp"),
	}})
	reach, err := probeFB.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	_, _, probeErr := c.resolveHomeExecutor(context.Background(), protocol.SpawnRequest{Cwd: "/tmp"}, probeBE, reach)
	ck.Require().Error(probeErr, "the probe refuses a cwd no machine holds")

	// The eager bind of a single reachable home, same error: it refuses too, and
	// with byte-identical text — one failure, one refusal message.
	bindFB := newFakeBinder(&fakeExec{id: "a"})
	bindBE := newBoundExecutor("c1", &probeBinder{fakeBinder: bindFB, provErr: map[string]error{
		"a": missingWorkdir("/tmp"),
	}})
	homeBound, cause, refusal := c.bindHome(context.Background(), bindBE, "a")
	ck.False(homeBound, "an unbindable home must not report bound")
	ck.Nil(cause, "a cwd refusal is not a connectivity cause")
	ck.Require().Error(refusal, "a cwd the home cannot see must refuse the spawn")
	ck.Eq(probeErr.Error(), refusal.Error(), "one refusal text for one failure, whichever rule found it")
	ck.StrContains(refusal.Error(), "workspace on executor a:", "refusal: %v", refusal)
}

// Every OTHER bind failure starts the child unbound, and the not-connected
// warning carries the reason so the diagnostic is not left in the daemon log.
func TestHomeExecutorBindFailureKeepsTheCause(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	be := newBoundExecutor("c1", &probeBinder{fakeBinder: fb, provErr: map[string]error{
		"a": connect.NewError(connect.CodeUnavailable, errors.New("executor link reset")),
	}})
	be.setHome("a")

	homeBound, bindErr, refusal := c.bindHome(context.Background(), be, "a")
	ck.False(homeBound, "a transport failure is not a bind")
	ck.Nil(refusal, "a transport failure must not refuse the spawn: it may heal")
	ck.Require().Error(bindErr, "the cause must be reported")

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, "a", false, bindErr, nil)
	ck.Require().Len(info.Warnings, 1, "warnings = %v", info.Warnings)
	ck.StrContains(info.Warnings[0], "home executor a is not connected", "warning: %q", info.Warnings[0])
	ck.StrContains(info.Warnings[0], "executor link reset",
		"the underlying reason must survive into the warning: %q", info.Warnings[0])
	ck.Eq("", info.Project, "no project tier from an unbindable home")
}

// The spawn path must ACT on bindHome's refusal: a single reachable home that
// cannot see the child's cwd refuses the spawn (the same text the probe gives a
// cwd no machine holds), and leaves no retained binding behind for a child that
// will never exist.
func TestHomeExecutorBadCwdStartsNoChild(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	c.binderForOverride = func(protocol.SpawnRequest, executorOwner) executorBinder {
		return &probeBinder{fakeBinder: newFakeBinder(&fakeExec{id: "a"}), provErr: map[string]error{
			"a": missingWorkdir("/tmp"),
		}}
	}

	var warnings []string
	_, err := c.agentRuntimeOptionsFor(baseRequest(), "c1", false, "brent", "", "", "", &warnings)
	ck.Require().Error(err, "a home that cannot see the cwd must refuse the spawn, not start it unbound")
	ck.StrContains(err.Error(), "workspace on executor a:", "refusal: %v", err)
	ck.StrContains(err.Error(), `workdir "/tmp" does not exist`, "refusal: %v", err)
	ck.Len(warnings, 0, "a refused spawn reports no warning")
	ck.Nil(c.boundExecutorFor("c1"), "no child will exist, so the retained binding must be dropped")
}

// The other half: a bind failure that may heal starts the child unbound, and its
// warning carries the reason into the operator's view and the caller's.
func TestHomeExecutorBindFailureStartsUnboundWithCause(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.execPool = &fakePool{}
	c.execStore = &fakeExecStore{execs: map[string]executors.Executor{}}
	c.binderForOverride = func(protocol.SpawnRequest, executorOwner) executorBinder {
		return &probeBinder{fakeBinder: newFakeBinder(&fakeExec{id: "a"}), provErr: map[string]error{
			"a": connect.NewError(connect.CodeUnavailable, errors.New("executor link reset")),
		}}
	}

	var warnings []string
	ro, err := c.agentRuntimeOptionsFor(baseRequest(), "c1", false, "brent", "", "", "", &warnings)
	ck.Require().NoError(err, "a transport failure must not refuse the spawn: it may heal")
	ck.NotNil(ro.Executor, "the whole workspace tier must stay present so the child can lazy-bind")
	ck.NotNil(ro.ProjectContext, "the pointer stays non-nil so the daemon's own cwd is never used")
	ck.Eq("", *ro.ProjectContext, "an unbindable home yields no project tier")
	ck.Require().Len(warnings, 1, "warnings = %v", warnings)
	ck.StrContains(warnings[0], "home executor a is not connected", "warning: %q", warnings[0])
	ck.StrContains(warnings[0], "executor link reset", "the cause must reach the caller: %q", warnings[0])
	wl, ok := c.takeWorkspaceLabels("c1")
	ck.True(ok, "the child must be marked unbound for Spawn's label merge")
	ck.Eq("unbound", wl.executorState, "executorState")
	ck.Eq("a", wl.homeExecutorID, "the resolved home is still recorded for a later resume")
}

// markInstalled gives c an install store holding one LIVE recipe per id — what
// "this executor was installed over ssh" means to the home resolver.
func markInstalled(t *testing.T, c *Controller, ids ...string) {
	t.Helper()
	c.install = newInstallState(installStoreWith(t, "u1", ids...), nil)
}

// An executor installed over ssh is never a home. Its lifecycle belongs to the
// installer — a relaunch replaces the remote process and every workspace on it —
// so the child's cwd, project instructions and skills must not live there.
func TestHomeExecutorNeverAnInstalledRow(t *testing.T) {
	t.Run("the cwd probe never provisions on an installed executor", func(t *testing.T) {
		ck := assert.NewAborting(t)
		fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"}, &fakeExec{id: "c"})
		pb := &probeBinder{fakeBinder: fb, provErr: map[string]error{
			"a": missingWorkdir("/tmp"),
			"c": missingWorkdir("/tmp"),
		}}
		be := newBoundExecutor("c1", pb)
		reach, err := fb.Reachable("c1")
		ck.Require().NoError(err, "Reachable")
		c := newTestController(t)
		markInstalled(t, c, "b")

		// b is not in provErr, so a probe there would SUCCEED — and become the
		// home. The refusal is what proves it was never asked.
		home, warnings, err := c.resolveHomeExecutor(context.Background(),
			protocol.SpawnRequest{Cwd: "/tmp"}, be, reach)
		ck.Require().Error(err, "with b excluded, no machine holds the cwd")
		ck.Eq("", home, "homeID")
		ck.Len(warnings, 0, "a refusal reports no warning")
		ck.Eq(0, fb.provisionsOn("b"), "an installed executor must never be probed for the child's cwd")
	})

	t.Run("an installed executor is not the single reachable home", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c, fb, be, reach := homeFixture(t, "a", "b")
		markInstalled(t, c, "b")

		home, warnings, err := c.resolveHomeExecutor(context.Background(),
			protocol.SpawnRequest{}, be, reach)
		ck.Require().NoError(err)
		ck.Eq("a", home, "the only home-capable machine is home; b is excluded")
		ck.Len(warnings, 0, "warnings")
		ck.Eq(0, fb.provisions, "no probe is needed once b is excluded")
	})

	t.Run("naming one refuses the spawn", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c, fb, be, reach := homeFixture(t, "a", "b")
		markInstalled(t, c, "b")

		home, warnings, err := c.resolveHomeExecutor(context.Background(),
			protocol.SpawnRequest{HomeExecutor: "b"}, be, reach)
		ck.Require().Error(err, "an installed executor can never be a home")
		ck.EqualError(err, "executor b was installed over ssh and cannot be a home executor")
		ck.Eq("", home, "homeID")
		ck.Len(warnings, 0, "a refused spawn reports no warning")
		ck.Eq(0, fb.provisions, "nothing may be provisioned for a refused home")
	})
}

// The "Your machine" candidate is chosen through the child's own row: a legacy
// row (nil set) reads its stored selector, never the request, which on a resume
// names nothing and would pick whichever executor sorts first.
func TestHomeInfoCandidateReadsTheLegacyRow(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c_legacy", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, Labels: map[string]string{}, ExecutorSelector: "machine=b",
	})
	be := &boundExecutor{childID: "c_legacy"}
	reach := []executors.Executor{{ID: "B", Labels: map[string]string{"machine": "b"}, Enabled: true}}

	info := c.homeInfo(context.Background(), protocol.SpawnRequest{}, executorOwner{}, be, "", false, nil, reach)

	ck.Require().NotNil(info.Workspace, "a legacy child gets its machine block")
	ck.Eq("b", info.Workspace.ExecutorName, "the block names the machine its stored selector reaches")
}
