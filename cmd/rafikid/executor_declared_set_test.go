package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// declaredFixture is a controller whose child "c1" holds set, built through the
// production row builders, with live as the connected executors.
func declaredFixture(t *testing.T, set []string, live ...execpool.LiveExecutor) (*Controller, *boundExecutor) {
	t.Helper()
	rows := map[string]executors.Executor{}
	for _, le := range live {
		rows[le.Executor.ID] = le.Executor
	}
	c := reachController("u1", live, rows, newFakeInstallStore())
	insertChildRow(c, "c1", "", "u1")
	assert.NewAborting(t).Require().NoError(c.st.SetExecutorSet("c1", set), "seed set")
	be := newBoundExecutor("c1", c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"}))
	return c, be
}

func resolveImplicit(be *boundExecutor) (string, bool, error) {
	return be.resolve(tools.WithExecutorTarget(context.Background(), &tools.ExecutorTarget{}))
}

func resolveNamed(be *boundExecutor, name string) (string, bool, error) {
	return be.resolve(tools.WithExecutorTarget(context.Background(), &tools.ExecutorTarget{Requested: name}))
}

// The set all always requires a target, even when one executor is connected: a
// call that omitted it must never land on whichever machine happens to be up.
func TestImplicitCallOfAnAllSetNeedsATargetWithOneLive(t *testing.T) {
	ck := assert.NewAborting(t)
	c, be := declaredFixture(t, []string{allSet}, nativeExec("A", "a", "u1"))

	id, multi, err := resolveImplicit(be)
	ck.Require().Error(err, "implicit call of an all child")
	ck.Eq("", id, "nothing resolves")
	ck.True(multi, "the call must name its target")
	ck.StrContains(err.Error(), `pass executor: "<name>"`, "the refusal tells the model what to do")
	ck.Len(c.execPool.(*fakePool).provisionCalls(), 0, "an ambiguous call provisions nothing")
	ck.True(be.Multi(), "notices treat the child as multi-executor")

	id, _, err = resolveNamed(be, "A")
	ck.Require().NoError(err, "naming the one live executor works")
	ck.Eq("A", id, "named target")
}

// Two declared ids stay ambiguous when a member disconnects, is disabled, or
// every member is lost: the declared set decides, never the live count, and the
// set on the row is untouched.
func TestImplicitCallOfATwoIDSetStaysAmbiguousAsMembersAreLost(t *testing.T) {
	ck := assert.NewAborting(t)
	a, b := nativeExec("A", "a", "u1"), nativeExec("B", "b", "u1")
	c, be := declaredFixture(t, []string{"A", "B"}, a, b)

	_, _, err := resolveImplicit(be)
	ck.Require().Error(err, "both connected")

	steps := []struct {
		name string
		live []execpool.LiveExecutor
	}{
		{"B disconnects", []execpool.LiveExecutor{a}},
		{"B is disabled", []execpool.LiveExecutor{a, disabled(b)}},
		{"every member is gone", nil},
	}
	for _, st := range steps {
		c.execPool = &fakePool{live: st.live}
		id, multi, err := resolveImplicit(be)
		ck.Require().Error(err, "%s: an implicit call still has to name its target", st.name)
		ck.Eq("", id, "%s: nothing resolves implicitly", st.name)
		ck.True(multi, "%s: still multi", st.name)
		snap, _ := c.st.Get("c1")
		ck.EqDeep([]string{"A", "B"}, snap.ExecutorSet, "%s: a loss never rewrites the set", st.name)
	}
}

// A set naming exactly one executor is the only declared shape that decides
// implicitly.
func TestImplicitCallOfASingleMemberSetResolves(t *testing.T) {
	ck := assert.NewAborting(t)
	_, be := declaredFixture(t, []string{"A"}, nativeExec("A", "a", "u1"), nativeExec("B", "b", "u1"))

	id, multi, err := resolveImplicit(be)
	ck.Require().NoError(err, "one declared member")
	ck.Eq("A", id, "resolved")
	ck.False(multi, "single")
}

// Ephemeral re-placement moves the home among the members of the set. It never
// rewrites the set and never makes an ambiguous set implicit.
func TestEphemeralReplacementNeverMakesAnAmbiguousSetImplicit(t *testing.T) {
	ck := assert.NewAborting(t)
	eph := func(id string) execpool.LiveExecutor {
		le := machineExec(id, id, nil)
		le.Executor.WorkspaceMode = "ephemeral"
		return le
	}
	c := setController(t, eph("E1"), eph("E2"))
	setParent(t, c, "c_parent", []string{allSet})
	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", WorkspaceMode: "ephemeral"})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"E1", "E2"}, snap.ExecutorSet, "both ephemeral members")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{eph("E2")}}
	be := newBoundExecutor(snap.ChildID, c.binderFor(protocol.SpawnRequest{WorkspaceMode: "ephemeral"}, executorOwner{}))
	_, _, err = resolveImplicit(be)
	ck.Require().Error(err, "E1 is lost; the call still names its target")
	ck.StrContains(err.Error(), "can reach 1 executor (E2)", "the survivor is listed, not chosen")

	after, _ := c.st.Get(snap.ChildID)
	ck.EqDeep([]string{"E1", "E2"}, after.ExecutorSet, "re-placement never changes the set")

	id, _, err := resolveNamed(be, "E2")
	ck.Require().NoError(err, "naming the surviving member works")
	ck.Eq("E2", id, "named target")
}

// An executor installed over ssh is never a home. The first workspace call of a
// child whose only reachable executor is an installed one runs there as an
// ordinary workspace and leaves the child without a home, as spawn-time
// resolution does.
func TestFirstCallNeverAdoptsAnInstalledExecutorAsHome(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "I")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("I", "inst", "u1"), nativeExec("O", "ord", "u1")},
		map[string]executors.Executor{"I": installedExecRow("I", "inst", "u1"), "O": installedExecRow("O", "ord", "u1")},
		installs)
	insertChildRow(c, "c_inst", "", "u1")
	ck.Require().NoError(c.st.SetExecutorSet("c_inst", []string{"I"}))
	insertChildRow(c, "c_ord", "", "u1")
	ck.Require().NoError(c.st.SetExecutorSet("c_ord", []string{"O"}))
	owner := executorOwner{Name: "u", UserID: "u1"}

	be := newBoundExecutor("c_inst", c.binderFor(protocol.SpawnRequest{}, owner))
	id, _, err := resolveImplicit(be)
	ck.Require().NoError(err, "the call proceeds on the installed executor")
	ck.Eq("I", id, "resolved")
	ck.Eq("", be.homeExecutor(), "an installed executor is not adopted as home")
	snap, _ := c.st.Get("c_inst")
	ck.Eq("", snap.Labels[homeExecutorLabel], "no home label is written")

	// The control: an ordinary executor in the same position is adopted, so the
	// assertion above is about the installed exclusion and not about adoption
	// never running here.
	be = newBoundExecutor("c_ord", c.binderFor(protocol.SpawnRequest{}, owner))
	id, _, err = resolveImplicit(be)
	ck.Require().NoError(err, "ordinary executor")
	ck.Eq("O", id, "resolved")
	ck.Eq("O", be.homeExecutor(), "an ordinary executor becomes home")
}

// A row written before sets existed has a nil set and a selector. Relaunching
// its script child reads that row, so the legacy selector still decides where it
// may launch; a request built without a set addresses nothing.
func TestLaunchOfALegacyChildReadsItsRow(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, exWithLaunch("L", map[string]string{"machine": "a"}, "", protocol.KindScript))
	c.st.Insert(&childstore.Session{
		ChildID: "c_legacy_script", Kind: protocol.KindScript, Status: protocol.StatusExited,
		ExecutorSelector: "machine=a", Labels: map[string]string{},
	})
	req := protocol.SpawnRequest{Kind: protocol.KindScript, ExecutorSelector: "machine=a"}

	_, err := c.chooseLaunchExecutor(req, executorOwner{}, protocol.KindScript)
	ck.Error(err, "without the row the request carries no set and addresses nothing")

	got, err := c.chooseLaunchExecutorFor(req, executorOwner{}, protocol.KindScript, "c_legacy_script")
	ck.Require().NoError(err, "with the row the legacy selector resolves")
	ck.Eq("L", got.ID, "launch executor")
}

func disabled(le execpool.LiveExecutor) execpool.LiveExecutor {
	le.Executor.Enabled = false
	return le
}

// enrolledOffline seeds an executor row the daemon knows (enrolled, owned) that
// is not connected and was not installed over ssh.
func enrolledOffline(c *Controller, id, machine, ownerUserID string) {
	c.execStore.(*fakeExecStore).execs[id] = executors.Executor{
		ID: id, Enabled: true, OwnerUserID: ownerUserID,
		Labels: map[string]string{"machine": machine},
	}
}

// A spawner whose set members are all offline (enrolled, not connected) still
// hands them on: the default set is a copy, never [] because a member is down,
// and an explicit name of an enrolled offline member is accepted.
func TestSpawnSetKeepsEnrolledOfflineMembers(t *testing.T) {
	ck := assert.NewAborting(t)
	c := reachController("u1", nil, nil, newFakeInstallStore())
	c.cm, c.native = newChildManager(), nil
	enrolledOffline(c, "OFF1", "off1", "u1")
	enrolledOffline(c, "OFF2", "off2", "u1")
	enrolledOffline(c, "FOREIGN", "foreign", "u2")
	insertChildRow(c, "c_parent", "", "u1")
	ck.Require().NoError(c.st.SetExecutorSet("c_parent", []string{"OFF1", "OFF2"}))
	owner := executorOwner{Name: "u", UserID: "u1"}

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", Kind: protocol.KindFundi}, owner)
	ck.Require().NoError(err, "default set")
	ck.EqDeep([]string{"OFF1", "OFF2"}, got.Executors, "the default is a copy of the parent's set, offline members included")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "off2", Kind: protocol.KindFundi}, owner)
	ck.Require().NoError(err, "naming an enrolled offline member")
	ck.EqDeep([]string{"OFF2"}, got.Executors, "named member")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"OFF1", "OFF2"}, Kind: protocol.KindFundi}, owner)
	ck.Require().NoError(err, "naming both")
	ck.EqDeep([]string{"OFF1", "OFF2"}, got.Executors, "named members")

	_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "FOREIGN", Kind: protocol.KindFundi}, owner)
	ck.Error(err, "another owner's executor is never offered, offline or not")
}

// The session executor `rafiki create` stands up is placement only: the child
// stores the spawner's whole reach and starts out working on it, and nothing it
// stores names the transient id. After the session executor is evicted the child
// still resolves its other executors.
func TestSessionExecutorSpawnStoresAllAndSurvivesEviction(t *testing.T) {
	ck := assert.NewAborting(t)
	sess := machineExec("sess-01", "laptop", map[string]string{"kind": "session", "owner": "u"})
	durable := machineExec("D", "server", nil)
	c := setController(t, sess, durable)

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ExecutorSelector: "machine=laptop"})
	ck.Require().NoError(err, "spawn with the session selector")
	ck.EqDeep([]string{allSet}, snap.ExecutorSet, "the session executor is never frozen into the set")
	ck.Eq("sess-01", snap.Labels[homeExecutorLabel], "it is the child's home")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{durable}}
	ck.EqDeep([]string{"D"}, reachFor(t, c, snap.ChildID, protocol.SpawnRequest{}), "after eviction the child still reaches its other executors")
}

// A child of a spawner that does not hold all cannot be widened by a session
// selector: it keeps the durable members that matched and refuses when only the
// transient executor did.
func TestSessionSelectorUnderARestrictedSpawnerNeverStoresTheSessionID(t *testing.T) {
	ck := assert.NewAborting(t)
	sess := machineExec("sess-01", "laptop", map[string]string{"kind": "session"})
	durable := machineExec("D", "laptop", nil)
	c := setController(t, sess, durable)
	setParent(t, c, "c_parent", []string{"sess-01", "D"})

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "machine=laptop"}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"D"}, got.Executors, "only the durable member is stored")

	setParent(t, c, "c_only", []string{"sess-01"})
	_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_only", ExecutorSelector: "machine=laptop"}, executorOwner{})
	ck.Require().Error(err, "a set made only of the transient executor is refused")
}

// The script launch path itself reads the child's row. The fixture has no
// daraja pool, so the launch that follows the choice fails; failing in the launch
// and naming the chosen executor is the evidence the choice succeeded. Any other
// outcome (a panic, a success, the choice's own refusal) fails the test.
func TestScriptLaunchOfALegacyChildChoosesFromItsRow(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, exWithLaunch("L", map[string]string{"machine": "a"}, "", protocol.KindScript))
	c.st.Insert(&childstore.Session{
		ChildID: "c_legacy_script", Kind: protocol.KindScript, Status: protocol.StatusExited,
		ExecutorSelector: "machine=a", Labels: map[string]string{},
	})
	req := protocol.SpawnRequest{
		Kind: protocol.KindScript, ExecutorSelector: "machine=a",
		Script: &protocol.ScriptSpec{Repo: "local", Script: "s"},
	}

	_, err := c.darajaScriptRunner(req, "c_legacy_script", "", "")
	ck.Require().Error(err, "the launch has no daraja pool to run on")
	ck.StrContains(err.Error(), "launch daraja on executor L:", "the executor was chosen from the row, and the failure is the launch's")
}

// A selector places the child on what can host it now. A stale enrolled row of
// the owner that matches the selector but is offline and not installed is not
// frozen into the set, or the child would be handed a machine that may never
// return and every call would have to name its target.
func TestSelectorPlacementDoesNotFreezeStaleEnrolledRows(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, nativeExec("A", "a", ""))
	c.execStore = newFakeExecStore()
	c.execStore.(*fakeExecStore).execs["STALE"] = executors.Executor{
		ID: "STALE", Enabled: true, Labels: map[string]string{"machine": "a"},
	}

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ExecutorSelector: "machine=a", Kind: protocol.KindFundi}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, got.Executors, "only the connected match is frozen")
}

// The daemon's own calls carry no target and nobody to ask for one: an all
// child with a single connected executor still gets its prefill read served,
// while the same child's tool call must name the target.
func TestInternalCallOfAnAllSetStillResolvesOnTheOnlyLiveExecutor(t *testing.T) {
	ck := assert.NewAborting(t)
	_, be := declaredFixture(t, []string{allSet}, nativeExec("A", "a", "u1"))

	id, _, err := be.resolve(context.Background())
	ck.Require().NoError(err, "an internal call has no target to omit")
	ck.Eq("A", id, "resolved on the only live executor")

	_, _, err = resolveImplicit(be)
	ck.Error(err, "the model's own call still has to name it")
}

// The spawn's pre-fill reads the child's cwd, which lives on its home executor,
// so a daemon-initiated read goes there even when the set names several; the
// model's own call with no target is still refused, and so is a pre-fill whose
// home is not reachable.
func TestPrefillReadsGoToTheHomeOfAnAmbiguousSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c, be := declaredFixture(t, []string{allSet}, nativeExec("A", "a", "u1"), nativeExec("B", "b", "u1"))
	be.setHome("B")
	prefill := func() (string, error) {
		id, _, err := be.resolve(tools.WithExecutorTarget(tools.WithHomeDefault(context.Background()), &tools.ExecutorTarget{HomeDefault: true}))
		return id, err
	}

	id, err := prefill()
	ck.Require().NoError(err, "pre-fill read")
	ck.Eq("B", id, "served by the home executor")

	_, _, err = resolveImplicit(be)
	ck.Error(err, "the model's own call with no target is still refused")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{nativeExec("A", "a", "u1")}}
	_, err = prefill()
	ck.Error(err, "a home that is not reachable is never replaced by a guess")
}

// A worker spawned with no executor by a child that is homed on the session
// executor never stores the transient id: after the terminal closes the worker
// still reaches the owner's other executors.
func TestSessionPlacedChildsWorkerSurvivesEviction(t *testing.T) {
	ck := assert.NewAborting(t)
	sess := machineExec("sess-01", "laptop", map[string]string{"kind": "session", "owner": "u"})
	durable := machineExec("D", "server", nil)
	c := setController(t, sess, durable)

	parent, err := spawnFundi(t, c, protocol.SpawnRequest{ExecutorSelector: "machine=laptop"})
	ck.Require().NoError(err, "session-placed parent")
	ck.Eq("sess-01", parent.Labels[homeExecutorLabel], "the parent is homed on the session executor")

	worker, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: parent.ChildID})
	ck.Require().NoError(err, "default worker spawn")
	ck.EqDeep([]string{allSet}, worker.ExecutorSet, "the worker never stores the session id")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{durable}}
	ck.EqDeep([]string{"D"}, reachFor(t, c, worker.ChildID, protocol.SpawnRequest{}), "after eviction the worker still reaches the other executor")
}

// A legacy parent (no stored set, a session-style selector) hands its default
// worker the durable matches only; one whose only match is the session
// executor is refused rather than given an empty set.
func TestLegacySessionParentsDefaultWorkerNeverStoresTheSessionID(t *testing.T) {
	ck := assert.NewAborting(t)
	sess := machineExec("sess-01", "laptop", map[string]string{"kind": "session"})
	durable := machineExec("D", "laptop", nil)
	c := setController(t, sess, durable)
	c.st.Insert(&childstore.Session{
		ChildID: "c_legacy", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		MaxDepth: 2, MaxChildren: 8, Labels: map[string]string{}, ExecutorSelector: "machine=laptop",
	})

	worker, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_legacy"})
	ck.Require().NoError(err, "default worker of a legacy session parent")
	ck.EqDeep([]string{"D"}, worker.ExecutorSet, "only the durable member is stored")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{sess}}
	_, err = spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_legacy"})
	ck.Require().Error(err, "a parent that reaches only the session executor cannot hand it on")
	ck.StrContains(err.Error(), "session executor", "the refusal says why")
}

// Under a finite spawner a session selector stores the durable matches and the
// spawn still succeeds, homed inside that set.
func TestSessionSelectorUnderARestrictedSpawnerSpawnsInsideTheSet(t *testing.T) {
	ck := assert.NewAborting(t)
	sess := machineExec("sess-01", "laptop", map[string]string{"kind": "session"})
	durable := machineExec("D", "laptop", nil)
	c := setController(t, sess, durable)
	setParent(t, c, "c_parent", []string{"sess-01", "D"})

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "machine=laptop"})
	ck.Require().NoError(err, "the spawn must not be refused for a home outside its set")
	ck.EqDeep([]string{"D"}, snap.ExecutorSet, "only the durable member")
	ck.NotEq("sess-01", snap.Labels[homeExecutorLabel], "the session executor is never the home of a finite set")
}
