package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/toolmeta"
)

// fakeExec is one executor's client. Each call returns the next queued error
// (nil means success), so a test can script "fail once, then succeed".
//
// parent, when set, is the fakeBinder that synthesized this exec via
// ChooseFor. Execute/StartJob then report their call counts on the binder,
// so a test that never captures the exec directly (it only knows an id
// ChooseFor made up on the fly) can still assert on call counts through
// f.executeCalls / f.startJobCalls.
type fakeExec struct {
	id      string
	parent  *fakeBinder
	mu      sync.Mutex
	errs    []error
	calls   int
	lastCmd string
}

func (f *fakeExec) next() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	e := f.errs[0]
	f.errs = f.errs[1:]
	return e
}

func (f *fakeExec) Execute(_ context.Context, tool string, _ json.RawMessage) (toolmeta.Result, error) {
	if f.parent != nil {
		f.parent.mu.Lock()
		f.parent.executeCalls++
		f.parent.mu.Unlock()
	}
	if err := f.next(); err != nil {
		return toolmeta.Result{}, err
	}
	return toolmeta.Result{Text: f.id + ":" + tool}, nil
}

func (f *fakeExec) StartJob(_ context.Context, command string) (string, error) {
	if f.parent != nil {
		f.parent.mu.Lock()
		f.parent.startJobCalls++
		f.parent.mu.Unlock()
	}
	if err := f.next(); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.lastCmd = command
	f.mu.Unlock()
	return f.id + ":job", nil
}

func (f *fakeExec) JobOutput(_ context.Context, _ string, _ int64) (tools.JobSnapshot, error) {
	if err := f.next(); err != nil {
		return tools.JobSnapshot{}, err
	}
	return tools.JobSnapshot{Found: true}, nil
}

func (f *fakeExec) KillJob(_ context.Context, _ string) error { return f.next() }
func (f *fakeExec) Ping(_ context.Context) error              { return f.next() }
func (f *fakeExec) ProjectContext(_ context.Context) (string, error) {
	return f.id + ":ctx", nil
}
func (f *fakeExec) ProjectSkills(_ context.Context) ([]skills.SkillMeta, error) { return nil, nil }
func (f *fakeExec) SkillBody(_ context.Context, name string) (string, string, error) {
	return f.id + ":" + name, "/dir", nil
}

// fakeBinder scripts what the daemon's executor plane tells a boundExecutor,
// and records the calls a test asserts on.
//
// Two views, deliberately separate, because production keeps them separate
// too: reachable is the LIVE set Reachable answers from (tests move it to
// simulate a machine connecting or departing), and ChooseFor's order is the
// legacy selection sequence some tests still drive.
type fakeBinder struct {
	mu          sync.Mutex
	order       []string // ChooseFor returns these in sequence
	chooseErr   error    // when set, ChooseFor always fails
	execs       map[string]*fakeExec
	liveByID    map[string]bool
	provisions  int
	releases    int
	noted       []string
	chooseCalls int
	nextExecID  int

	// reachable is what Reachable answers with, in this order. Tests move ids
	// in and out of it between calls: the set is LIVE, never cached.
	reachable []string
	reachErr  error

	// machines maps an executor id to its "machine" label, which is what
	// DisplayName reports. A missing entry reads as the id itself.
	machines map[string]string

	// homeLabel is what Home answers for any child (one child per binder in
	// these tests). SetHomeLabel writes it, mirroring the real row.
	homeLabel string

	// homeLabels records SetHomeLabel calls as "child|executor" in call order.
	homeLabels []string

	// useCwdByExec records, per executor id, the useCwd flag of every
	// ProvisionOn call, so a test can pin which workspaces get the child's cwd.
	useCwdByExec map[string][]bool

	// mode/live back WorkspaceMode/IsLive for tests that care about the
	// pinned/ephemeral policy rather than about wiring up fakeExecs by hand.
	// live defaults true (see newFakeBinder) so the pre-existing per-id
	// liveByID map keeps deciding liveness unless a test overrides it.
	mode string
	live bool

	// migrations/lastSteer record NotifyMigrated calls.
	migrations int
	lastSteer  string
	lastFrom   string
	lastTo     string

	// forgotRunning records ForgetRunningJob calls, as "executorID|handle"
	// pairs in call order. The daemon prunes a self-killed job from its running
	// set through this so a later ReplayComplete never calls it "gone".
	forgotRunning []string

	// failWith/failTimes script a failure returned by a freshly synthesized
	// executor's client (Execute/StartJob), for tests that want a scripted
	// failure without wiring up a fakeExec by hand -- the id ChooseFor
	// synthesizes is never captured by the test. failWith is returned for
	// the first failTimes calls; if failTimes is zero, exactly one call
	// fails. executeCalls/startJobCalls count those calls across every
	// synthesized exec, again because the test never has an id to key on.
	failWith      error
	failTimes     int
	executeCalls  int
	startJobCalls int

	// lastWorkspace is the workspace id returned by the most recent
	// ProvisionOn call. released records which workspace ids ReleaseOn was
	// called for.
	lastWorkspace string
	released      map[string]bool

	// onRelease, when set, runs synchronously inside ReleaseOn -- with f.mu NOT
	// held -- so a test can hold one release open and prove teardown does not
	// serialise them.
	onRelease func(execID string)

	// onProvision, when set, runs synchronously inside ProvisionOn -- with
	// f.mu NOT held -- before it does anything else. It lets a test hold a
	// Provision RPC open to prove a caller (recover) isn't holding
	// boundExecutor's lock across it.
	onProvision func(execID string)

	// clientOverride, when non-nil, is what ProvisionOn returns instead of a
	// registered fakeExec. The owner-propagation test needs a client that
	// records the ctx each bound call is made with, not a scripted tool exec.
	clientOverride tools.ExecutorClient

	// clientByExec overrides the client for ONE executor, leaving every other
	// executor's registered fakeExec alone. A test that scripts a burst of
	// simultaneous failures on a must not thereby script b's calls too.
	clientByExec map[string]tools.ExecutorClient
}

func newFakeBinder(execs ...*fakeExec) *fakeBinder {
	// mode defaults to "ephemeral" so the pre-existing rebind tests, which
	// predate workspace_mode and never set it, keep exercising a migration.
	// Tests that care about the pinned policy set f.mode explicitly.
	fb := &fakeBinder{
		execs:        map[string]*fakeExec{},
		liveByID:     map[string]bool{},
		machines:     map[string]string{},
		useCwdByExec: map[string][]bool{},
		clientByExec: map[string]tools.ExecutorClient{},
		live:         true,
		mode:         "ephemeral",
		released:     map[string]bool{},
	}
	for _, e := range execs {
		fb.execs[e.id] = e
		fb.liveByID[e.id] = true
		fb.order = append(fb.order, e.id)
		fb.reachable = append(fb.reachable, e.id)
	}
	return fb
}

func (f *fakeBinder) ChooseFor(string) (string, error) {
	f.mu.Lock()
	if f.chooseErr != nil {
		defer f.mu.Unlock()
		f.chooseCalls++
		return "", f.chooseErr
	}
	if len(f.order) == 0 {
		// Nothing was explicitly configured: synthesize a fresh executor each
		// time, so a policy test can tell a re-selection (a NEW id) apart from
		// an in-place re-provision (the SAME id) without wiring up fakeExecs.
		f.chooseCalls++
		f.nextExecID++
		id := fmt.Sprintf("exec-%d", f.nextExecID)
		e := &fakeExec{id: id, parent: f}
		if f.failWith != nil {
			n := f.failTimes
			if n == 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				e.errs = append(e.errs, f.failWith)
			}
		}
		f.execs[id] = e
		f.liveByID[id] = true
		// A synthesized executor is one that just CONNECTED: put it in the live
		// set so the next call resolves to it instead of asking again.
		f.reachable = append(f.reachable, id)
		f.mu.Unlock()
		return id, nil
	}
	id := f.order[0]
	if len(f.order) > 1 {
		f.order = f.order[1:]
	}
	f.chooseCalls++
	f.mu.Unlock()
	return id, nil
}

// Reachable answers with the live set the test has configured. Freshly, every
// call: TestBoundExecutorReachableReevaluatedPerCall moves ids in and out
// between calls and fails if this ever answers a cached set.
func (f *fakeBinder) Reachable(string) ([]executors.Executor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reachErr != nil {
		return nil, f.reachErr
	}
	out := make([]executors.Executor, 0, len(f.reachable))
	for _, id := range f.reachable {
		out = append(out, executors.Executor{
			ID:      id,
			Enabled: true,
			Labels:  map[string]string{"machine": f.machineLocked(id)},
		})
	}
	// Mirror sortCandidates' order for ordinary (non-sandbox, non-session)
	// rows: by id.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeBinder) machineLocked(id string) string {
	if m, ok := f.machines[id]; ok && m != "" {
		return m
	}
	return id
}

func (f *fakeBinder) DisplayName(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return shortID(f.machineLocked(id))
}

func (f *fakeBinder) Home(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.homeLabel
}

func (f *fakeBinder) SetHomeLabel(childID, execID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.homeLabel = execID
	f.homeLabels = append(f.homeLabels, childID+"|"+execID)
}

func (f *fakeBinder) ProvisionOn(_ context.Context, id string, useCwd bool) (string, string, tools.ExecutorClient, error) {
	f.mu.Lock()
	hook := f.onProvision
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisions++
	f.useCwdByExec[id] = append(f.useCwdByExec[id], useCwd)
	// The executor's own answer: the child's cwd where it applies, else its
	// root. The name is the display name so a Note reads the way a model does.
	workdir := "/root/" + f.machineLocked(id)
	if useCwd {
		workdir = "/cwd"
	}
	if f.clientOverride != nil {
		ws := fmt.Sprintf("ws-%s-%d", id, f.provisions)
		f.lastWorkspace = ws
		return ws, workdir, f.clientOverride, nil
	}
	if override, ok := f.clientByExec[id]; ok {
		ws := fmt.Sprintf("ws-%s-%d", id, f.provisions)
		f.lastWorkspace = ws
		return ws, workdir, override, nil
	}
	e, ok := f.execs[id]
	if !ok {
		return "", "", nil, fmt.Errorf("no such executor %q", id)
	}
	// Each call gets a DISTINCT workspace id, matching a real executor: it
	// mints a fresh workspace every time, even for the same executor. A
	// deterministic "ws-"+id here would make an in-place re-provision look
	// like it returned the SAME workspace, hiding the old one never having
	// been released.
	ws := fmt.Sprintf("ws-%s-%d", id, f.provisions)
	f.lastWorkspace = ws
	return ws, workdir, e, nil
}

func (f *fakeBinder) ReleaseOn(_ context.Context, executorID, workspaceID string) {
	f.mu.Lock()
	hook := f.onRelease
	f.mu.Unlock()
	if hook != nil {
		hook(executorID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	f.released[workspaceID] = true
}

func (f *fakeBinder) IsLive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live && f.liveByID[id]
}

func (f *fakeBinder) NoteBinding(_, execID, wsID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noted = append(f.noted, execID+"/"+wsID)
}

// WorkspaceMode returns the scripted mode for every executor. A single value
// is enough for these tests: they exercise the pinned/ephemeral decision, not
// per-executor mode variation.
func (f *fakeBinder) WorkspaceMode(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

func (f *fakeBinder) NotifyMigrated(_, from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrations++
	f.lastSteer = rescheduleSteer
	f.lastFrom, f.lastTo = from, to
}

func (f *fakeBinder) ForgetRunningJob(executorID, handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotRunning = append(f.forgotRunning, executorID+"|"+handle)
}

// --- helpers the reachable-set tests share ---

// reachableTo replaces the live executor set. Fresh on every Reachable call,
// exactly as the daemon computes it.
func (f *fakeBinder) reachableTo(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable = append([]string(nil), ids...)
}

// useCwdFor is the useCwd flag of every ProvisionOn call against id.
func (f *fakeBinder) useCwdFor(id string) []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.useCwdByExec[id]...)
}

// provisionsOn counts ProvisionOn calls against one executor.
func (f *fakeBinder) provisionsOn(id string) int {
	return len(f.useCwdFor(id))
}

// namedCall is a tool call's context as the registry wrapper builds it: the
// model named an executor on this call.
func namedCall(executor string) context.Context {
	return tools.WithExecutorTarget(context.Background(), &tools.ExecutorTarget{Requested: executor})
}

func TestBoundExecutorBindsLazilyAndSticks(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeExec{id: "a"}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	for i := 0; i < 3; i++ {
		_, err := b.Execute(context.Background(), "read", nil)
		c.NoError(err, "call %d", i)
	}
	c.Eq(1, fb.provisions, "a working executor must be provisioned ONCE and held; got")
}

func TestBoundExecutorDoesNotRebindOnToolFailure(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeExec{id: "a", errs: []error{fmt.Errorf("wrapped: %w", execpool.ErrToolFailed)}}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "bash", nil)
	c.ErrorIs(err, execpool.ErrToolFailed, "a tool failure must be returned to the caller unchanged, got")
	c.Eq(1, fb.provisions, "bash exiting nonzero must NEVER migrate a child; got")
}

// A departed executor must be replaced, not surfaced: an ephemeral child
// follows its work onto the machine that is still there.
func TestBoundExecutorRebindsWhenTheExecutorIsGone(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeExec{id: "a"}
	b2 := &fakeExec{id: "b"}
	fb := newFakeBinder(a, b2)
	// Only a is reachable to start with, so the implicit call has one answer.
	fb.reachableTo("a")
	b := newBoundExecutor("child-1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	// a departs: it leaves the live set, and b is now the only machine.
	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	gotRes, err := b.Execute(context.Background(), "read", nil)
	got := gotRes.Text
	c.NoError(err, "a departed executor must be replaced, not surfaced")
	c.Eq("b:read", got, "the retry must run on the NEW executor; got")
	c.Eq(1, fb.provisionsOn("b"), "b provisions")
}

func TestBoundExecutorReprovisionsOnSameExecutorWhenOnlyWorkspaceIsGone(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeExec{id: "a"}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	chooseBefore := fb.chooseCalls
	fb.mu.Unlock()
	// a stays LIVE; only its in-memory workspace registry lost the id.
	a.errs = []error{fmt.Errorf("wrapped: %w", execpool.ErrExecutorGone)}

	_, err := b.Execute(context.Background(), "read", nil)
	c.NoError(err, "a lost workspace on a live executor must be re-provisioned")
	c.Eq(2, fb.provisions, "want exactly one re-provision, got")
	fb.mu.Lock()
	chooseAfter := fb.chooseCalls
	fb.mu.Unlock()
	c.Eq(chooseBefore, chooseAfter, "a lost WORKSPACE must not re-run selection; ChooseFor called")
}

func TestBoundExecutorRetriesOnlyOnce(t *testing.T) {
	c := assert.NewAborting(t)
	a := &fakeExec{id: "a", errs: []error{
		fmt.Errorf("1: %w", execpool.ErrExecutorGone),
		fmt.Errorf("2: %w", execpool.ErrExecutorGone),
		fmt.Errorf("3: %w", execpool.ErrExecutorGone),
	}}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "read", nil)
	c.Error(err, "an executor failing every time must surface the error, not loop")
	c.LessOrEqual(2, a.calls, "at most one retry per call; the executor saw")
}

// burstExec is a client whose first n Execute calls all fail at the SAME
// moment: every caller parks on a barrier until n calls have arrived.
//
// The simultaneity is the point. Coalescing is only proved by failures that
// land together — a caller whose first call happens to be dispatched AFTER a
// rebuild is failing against a NEW workspace and may legitimately provoke
// another re-provision, so n goroutines merely started together prove nothing.
type burstExec struct {
	n       int
	barrier chan struct{}
	mu      sync.Mutex
	arrived int
	ready   bool // the first wave has all arrived; later calls are retries
}

func newBurstExec(n int) *burstExec {
	return &burstExec{n: n, barrier: make(chan struct{})}
}

func (e *burstExec) Execute(_ context.Context, tool string, _ json.RawMessage) (toolmeta.Result, error) {
	e.mu.Lock()
	// A call that arrives BEFORE the wave is complete belongs to it, whatever
	// order the scheduler runs them in; anything after is a retry and succeeds.
	firstWave := !e.ready
	e.arrived++
	if firstWave && e.arrived == e.n {
		e.ready = true
		close(e.barrier)
	}
	wait := e.barrier
	e.mu.Unlock()
	<-wait

	if firstWave {
		return toolmeta.Result{}, fmt.Errorf("gone: %w", execpool.ErrExecutorGone)
	}
	return toolmeta.Result{Text: "served:" + tool}, nil
}

func (e *burstExec) StartJob(context.Context, string) (string, error) { return "job", nil }
func (e *burstExec) JobOutput(context.Context, string, int64) (tools.JobSnapshot, error) {
	return tools.JobSnapshot{Found: true}, nil
}
func (e *burstExec) KillJob(context.Context, string) error { return nil }
func (e *burstExec) Ping(context.Context) error            { return nil }

func TestBoundExecutorConcurrentFailuresProduceOneRebind(t *testing.T) {
	c := assert.NewAborting(t)
	const n = 8
	burst := newBurstExec(n)
	fb := newFakeBinder()
	fb.clientOverride = burst
	b := newBoundExecutor("child-1", fb)

	// Bind first, so all n failures land against the SAME workspace.
	c.NoError(b.bindImplicit(context.Background()))
	c.Eq(1, fb.provisions, "the initial bind provisions once; got")

	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.Execute(context.Background(), "read", nil)
			if err != nil {
				results[i] = "ERR: " + err.Error()
			} else {
				results[i] = res.Text
			}
		}()
	}
	wg.Wait()

	// One provision at bind + exactly one re-provision. Eight simultaneous
	// failures must not provision eight workspaces.
	c.Eq(2, fb.provisions, "concurrent failures must coalesce into ONE re-provision; got")
	want := make([]string, n)
	for i := range want {
		want[i] = "served:read"
	}
	c.EqDeep(want, results, "every caller must be served by the retry")
}

func TestBoundExecutorUnboundReturnsTheRefusalReason(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder()
	fb.chooseErr = errors.New("spawn refused: no executor satisfies \"machine=xyz\"")
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "read", nil)
	c.Error(err, "an unbound executor must error")
	c.True(contains(err.Error(), "no executor satisfies"), "the refusal diagnostic must reach the agent, got %q", err)
}

func TestBoundExecutorNeverRunsInProcess(t *testing.T) {
	// The security invariant from the design doc: opts.Executor is always
	// non-nil now, which bypasses MaterializeAll's nil check — the guard that
	// stops workspace tools running in the daemon. boundExecutor is what
	// makes that safe, and only if it ERRORS rather than falling back.
	fb := newFakeBinder()
	fb.chooseErr = errors.New("nothing live")
	b := newBoundExecutor("child-1", fb)

	if _, err := b.Execute(context.Background(), "read", []byte(`{"path":"/etc/hosts"}`)); err == nil {
		t.Fatal("an unresolvable boundExecutor MUST error; falling back to " +
			"in-process execution is the confinement escape this type exists to prevent")
	}
	if _, err := b.StartJob(context.Background(), "echo hi"); err == nil {
		t.Fatal("StartJob must refuse when unbound")
	}
	assert.NewAborting(t).Error(b.Ping(context.Background()), "Ping must refuse when unbound")
}

// Re-provisioning in place leaves the OLD workspace registered on a LIVE
// executor. Unlike the migration branch below it, nothing released it -- so it
// and its retained background-job output were stranded until the executor
// process exited.
func TestReprovisionInPlaceReleasesTheOldWorkspace(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = true
	b := newBoundExecutor("c1", f)
	c.NoError(b.bindImplicit(context.Background()))
	first := f.lastWorkspace

	execID, wsID := b.stale()
	c.True(b.recover(context.Background(), execID, wsID, execpool.ErrExecutorGone, true), "want a re-provision")
	if !f.released[first] {
		t.Fatalf("workspace %q was never released; it and its job output are "+
			"stranded on a live executor", first)
	}
}

// recover must not hold b.mu across a network call to an executor that is by
// definition unwell, or every other tool call on that child blocks for the full
// RPC timeout.
func TestRecoverDoesNotHoldTheLockAcrossProvision(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	entered := make(chan struct{})
	release := make(chan struct{})
	f.onProvision = func(string) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	b := newBoundExecutor("c1", f)
	// Bind with the hook REMOVED: the first bind is allowed to run under the
	// lock, and this test is about recover.
	f.mu.Lock()
	hook := f.onProvision
	f.onProvision = nil
	f.mu.Unlock()
	if err := b.bindImplicit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.onProvision = hook
	f.mu.Unlock()
	execID, wsID := b.stale()

	go func() { b.recover(context.Background(), execID, wsID, execpool.ErrExecutorLost, true) }()
	<-entered

	done := make(chan struct{})
	go func() { defer close(done); b.Current() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Current() blocked behind an in-flight Provision; a slow executor " +
			"must not wedge every other caller on this child")
	}
	close(release)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A successful kill drops the job from the daemon's running set so a later
// ReplayComplete never reports the job the agent killed as "gone" — gone means
// an executor restart lost it, not "you killed it".
func TestKillJobForgetsTheRunningJob(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder()
	b := newBoundExecutor("child-1", fb)

	handle, err := b.StartJob(context.Background(), "npm run dev")
	c.NoError(err)
	execID, _, bound := b.Current()
	c.True(bound, "StartJob must leave the child bound")
	c.NoError(b.KillJob(context.Background(), handle))

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.forgotRunning) != 1 || fb.forgotRunning[0] != execID+"|"+handle {
		t.Fatalf("forgotRunning = %v, want exactly [%s|%s]", fb.forgotRunning, execID, handle)
	}
}

// A kill that never landed (the stream broke) drops nothing: the job may
// still be running, and its exit is still news.
func TestFailedKillJobForgetsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder()
	fb.failWith = errors.New("stream broken")
	b := newBoundExecutor("child-1", fb)

	c.Error(b.KillJob(context.Background(), "job-1"), "expected the scripted failure")
	fb.mu.Lock()
	defer fb.mu.Unlock()
	c.Empty(fb.forgotRunning, "forgotRunning")
}

// ownerRecorderExec is a tools.ExecutorClient that records the owner carried by
// each call's context, via the exported execpool.OwnerFrom. It is the pin F1
// needed: the daemon must tag EVERY bound executor call with the child's id —
// if callBound wraps only its own ctx copy while a closure closes over the
// caller's ctx, OwnerFrom here reads "" and the feature is silently dead.
type ownerRecorderExec struct {
	mu     sync.Mutex
	owners []string
	errs   []error // scripted per-call errors, consumed in order
}

func (f *ownerRecorderExec) record(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners = append(f.owners, execpool.OwnerFrom(ctx))
	if len(f.errs) == 0 {
		return nil
	}
	e := f.errs[0]
	f.errs = f.errs[1:]
	return e
}

func (f *ownerRecorderExec) Execute(ctx context.Context, _ string, _ json.RawMessage) (toolmeta.Result, error) {
	return toolmeta.Result{Text: "ok"}, f.record(ctx)
}
func (f *ownerRecorderExec) StartJob(ctx context.Context, _ string) (string, error) {
	if err := f.record(ctx); err != nil {
		return "", err
	}
	return "job-1", nil
}
func (f *ownerRecorderExec) JobOutput(ctx context.Context, _ string, _ int64) (tools.JobSnapshot, error) {
	if err := f.record(ctx); err != nil {
		return tools.JobSnapshot{}, err
	}
	return tools.JobSnapshot{Found: true}, nil
}
func (f *ownerRecorderExec) KillJob(ctx context.Context, _ string) error { return f.record(ctx) }
func (f *ownerRecorderExec) Ping(ctx context.Context) error              { return f.record(ctx) }

func (f *ownerRecorderExec) recordedOwners() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.owners...)
}

// Every bound method must carry the child as the executor call's owner. This is
// the end-to-end pin at the boundExecutor level: it would have caught the bug
// where callBound tagged its own ctx copy while each closure used the caller's
// untagged ctx (OwnerFrom read "" on every production call).
func TestBoundExecutorCallCarriesTheOwnerOnEveryMethod(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := &ownerRecorderExec{}
	fb := newFakeBinder()
	fb.clientOverride = rec
	b := newBoundExecutor("child-owner", fb)
	ctx := context.Background()

	if _, err := b.Execute(ctx, "read", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := b.StartJob(ctx, "ls"); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if _, err := b.JobOutput(ctx, "job-1", 0); err != nil {
		t.Fatalf("JobOutput: %v", err)
	}
	if err := b.KillJob(ctx, "job-1"); err != nil {
		t.Fatalf("KillJob: %v", err)
	}

	owners := rec.recordedOwners()
	ck.Len(owners, 4, "one recorded owner per call")
	for i, o := range owners {
		ck.Eq("child-owner", o, "call %d must carry the child as the executor call's owner", i)
	}
}

// The owner must also ride the recover RETRY, not just the first attempt.
func TestBoundExecutorRetryCarriesTheOwner(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := &ownerRecorderExec{errs: []error{fmt.Errorf("gone: %w", execpool.ErrExecutorLost)}}
	fb := newFakeBinder()
	fb.clientOverride = rec
	b := newBoundExecutor("child-retry", fb)

	// "read" is idempotent, so a liveness failure triggers exactly one retry
	// against the re-provisioned (same, live) executor.
	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("the idempotent read must succeed on the retry: %v", err)
	}

	ck.EqDeep([]string{"child-retry", "child-retry"}, rec.recordedOwners(),
		"both the first attempt and the recover retry must carry the owner")
}

// --- the reachable, per-call executor set ------------------------------------

// One reachable executor needs no argument: the call goes there, and that
// executor becomes the child's HOME — the one whose workspace its cwd, project
// tier and labels belong to.
func TestBoundExecutorSingleReachableNeedsNoArg(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	b := newBoundExecutor("c1", fb)

	tg := &tools.ExecutorTarget{}
	res, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil)
	c.NoError(err, "one reachable executor and no argument must resolve")
	c.Eq("a:read", res.Text, "served on")
	c.Eq("a", tg.Resolved, "Resolved")
	c.False(tg.Multi, "one reachable executor is not multi")
	c.Eq("", tg.Note, "the HOME binding gets no note: its cwd is the child's")
	c.Eq(1, fb.provisionsOn("a"), "provisions on a")
	fb.mu.Lock()
	noted := append([]string(nil), fb.noted...)
	fb.mu.Unlock()
	c.Len(noted, 1, "the adopted home binding must own the labels; noted")
	c.StrContains(noted[0], "a/", "noted value")
	execID, _, bound := b.Current()
	c.True(bound, "bound")
	c.Eq("a", execID, "Current")
}

// Two reachable and no argument is AMBIGUOUS, and the error has to enumerate
// the machines or the model's only possible next move is a guess.
func TestBoundExecutorAmbiguousCallListsExecutors(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "ublk-s1"}, &fakeExec{id: "ublk-c1"})
	b := newBoundExecutor("c1", fb)

	_, err := b.Execute(context.Background(), "read", nil)
	c.Error(err, "two reachable executors and no argument must refuse")
	c.Eq(`this agent can reach 2 executors (ublk-c1, ublk-s1); pass executor: "<name>"`, err.Error(), "error text")
	c.Eq(0, fb.provisions, "an ambiguous call must provision NOTHING; got")
}

// A named call goes to the named machine and nowhere else.
func TestBoundExecutorNamedExecutorRoutesThere(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "ublk-s1"}, &fakeExec{id: "ublk-c1"})
	b := newBoundExecutor("c1", fb)

	tg := &tools.ExecutorTarget{Requested: "ublk-s1"}
	res, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil)
	c.NoError(err)
	c.Eq("ublk-s1:read", res.Text, "served on")
	c.Eq("ublk-s1", tg.Resolved, "Resolved")
	c.True(tg.Multi, "two reachable executors means Multi")
	c.Eq(1, fb.provisionsOn("ublk-s1"), "provisions on the named executor")
	c.Eq(0, fb.provisionsOn("ublk-c1"), "provisions on the machine it did NOT name")
}

// A name this agent cannot reach says so, and lists what it CAN reach — an
// error the model can act on.
func TestBoundExecutorUnreachableNameRefused(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "ublk-s1"}, &fakeExec{id: "ublk-c1"})
	b := newBoundExecutor("c1", fb)

	tg := &tools.ExecutorTarget{Requested: "nope"}
	_, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil)
	c.Error(err, "an unreachable name must refuse")
	c.Eq(`executor "nope" is not reachable by this agent; reachable: ublk-c1, ublk-s1`, err.Error(), "error text")
	c.Eq("", tg.Resolved, "a refused call resolves nothing")

	fb.reachableTo()
	tg = &tools.ExecutorTarget{Requested: "nope"}
	_, err = b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil)
	c.Error(err, "an empty reachable set must still refuse")
	c.Eq(`executor "nope" is not reachable by this agent; reachable: none`, err.Error(), "error text")
}

// The set is LIVE: an executor that connects after the child started is usable
// on the next call, and one that departs stops being usable immediately. A
// cached set makes one of these three calls disagree with reality.
func TestBoundExecutorReachableReevaluatedPerCall(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	b := newBoundExecutor("c1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("one reachable executor must work: %v", err)
	}

	fb.reachableTo("a", "b")
	_, err := b.Execute(context.Background(), "read", nil)
	c.Error(err, "b connected, so an unqualified call is now ambiguous")
	c.StrContains(err.Error(), "can reach 2 executors", "the ambiguity must be reported; got %q", err)

	fb.reachableTo("a")
	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("b departed, so the call must work again: %v", err)
	}
	c.Eq(1, fb.provisionsOn("a"), "a's workspace is reused, not re-provisioned")
	c.Eq(0, fb.provisionsOn("b"), "nothing was ever provisioned on b")
}

// One workspace per executor, made lazily and then held.
func TestBoundExecutorLazyProvisionPerExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	b := newBoundExecutor("c1", fb)

	for _, name := range []string{"a", "b", "a", "b"} {
		if _, err := b.Execute(namedCall(name), "read", nil); err != nil {
			t.Fatalf("call naming %q: %v", name, err)
		}
	}
	c.Eq(1, fb.provisionsOn("a"), "provisions on a")
	c.Eq(1, fb.provisionsOn("b"), "provisions on b")
	c.Eq(2, fb.provisions, "total provisions")
	c.Len(b.Bindings(), 2, "bindings")
}

// The child's cwd belongs to ONE machine. A workspace at another machine's root
// is what makes a named call land in that machine's world instead of in a path
// that does not exist there — and the model is told once, on the first call.
func TestBoundExecutorNonHomeStartsAtRoot(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	b := newBoundExecutor("c1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("implicit call: %v", err)
	}
	c.EqDeep([]bool{true}, fb.useCwdFor("a"), "the HOME workspace gets the child's cwd")

	fb.reachableTo("a", "b")
	tg := &tools.ExecutorTarget{Requested: "b"}
	if _, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil); err != nil {
		t.Fatalf("named call: %v", err)
	}
	c.EqDeep([]bool{false}, fb.useCwdFor("b"), "another machine's workspace starts at ITS root")
	c.Eq("[b] working from /root/b", tg.Note, "the first call on a fresh non-home binding must say where it works")

	tg2 := &tools.ExecutorTarget{Requested: "b"}
	if _, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg2), "read", nil); err != nil {
		t.Fatalf("second named call: %v", err)
	}
	c.Eq("", tg2.Note, "the note is one-time")
}

// Only the home binding writes labels. handleChildExit releases a workspace by
// snap.Labels, so a second binding claiming them would both lie about the
// child's tree and steal the release of the real one.
func TestBoundExecutorOnlyHomeWritesLabels(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	b := newBoundExecutor("c1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("implicit call: %v", err)
	}
	fb.reachableTo("a", "b")
	if _, err := b.Execute(namedCall("b"), "read", nil); err != nil {
		t.Fatalf("named call: %v", err)
	}
	fb.mu.Lock()
	noted := append([]string(nil), fb.noted...)
	fb.mu.Unlock()
	c.Len(noted, 1, "exactly one label write, for home; got")
	c.StrContains(noted[0], "a/", "the one note names the home executor")
}

// Recovery coalesces PER BINDING: eight simultaneous failures on a rebuild a
// once, while a call on b neither blocks behind that rebuild nor re-provisions
// b. The cross-binding half is the new property — a single global inflight
// would make one bad machine stall every other call on every other machine.
func TestBoundExecutorRecoveryCoalescedPerBinding(t *testing.T) {
	c := assert.NewAborting(t)
	const n = 8
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a", "b")
	// a's client fails exactly the n simultaneous calls this test makes.
	fb.mu.Lock()
	fb.clientByExec["a"] = newBurstExec(n)
	fb.mu.Unlock()
	b := newBoundExecutor("c1", fb)
	b.setHome("a")
	if _, err := b.clientFor(context.Background(), "a"); err != nil {
		t.Fatalf("bind a: %v", err)
	}
	if _, err := b.Execute(namedCall("b"), "read", nil); err != nil {
		t.Fatalf("named call on b: %v", err)
	}

	fb.mu.Lock()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.onProvision = func(execID string) {
		if execID != "a" {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	}
	fb.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Execute(namedCall("a"), "read", nil)
		}()
	}
	<-entered

	// b's call must complete while a's re-provision is still in flight.
	done := make(chan error, 1)
	go func() {
		_, err := b.Execute(namedCall("b"), "read", nil)
		done <- err
	}()
	select {
	case err := <-done:
		c.NoError(err, "a call on a healthy binding must not block behind another binding's rebuild")
	case <-time.After(2 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("a call on b blocked behind a's in-flight re-provision; recovery is holding a lock it must not")
	}
	close(release)
	wg.Wait()

	c.Eq(2, fb.provisionsOn("a"), "eight simultaneous failures on a must produce ONE re-provision; got")
	c.Eq(1, fb.provisionsOn("b"), "b was never re-provisioned; got")
}

// A pinned home fails where it stood: it is not re-bound to a different
// executor, and no workspace is made on the machine that is still there.
func TestBoundExecutorPinnedLostDoesNotMigrate(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	fb.mode = "pinned"
	b := newBoundExecutor("c1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("implicit call: %v", err)
	}
	// a departs; b is the only machine left.
	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	_, err := b.Execute(context.Background(), "read", nil)
	c.Error(err, "a pinned child must fail where it stood")
	c.Eq("executor a is not connected; this agent's workspace is pinned there, so calls fail until it reconnects", err.Error(), "error text")
	c.Eq(0, fb.provisionsOn("b"), "a pinned child must NOT be provisioned on another machine; got")
	c.Eq(0, fb.migrations, "a pinned child must not be told it migrated; migrations")
}

// An EPHEMERAL home migrates on an implicit call and is told about it; a call
// that NAMED the departed executor never migrates — the model asked for that
// machine.
func TestBoundExecutorEphemeralMigratesImplicitOnly(t *testing.T) {
	c := assert.NewAborting(t)

	// Implicit: the child follows its work.
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	fb.mode = "ephemeral"
	b := newBoundExecutor("c1", fb)
	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("implicit call: %v", err)
	}
	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	res, err := b.Execute(context.Background(), "read", nil)
	c.NoError(err, "an ephemeral child follows its work onto the machine that is there")
	c.Eq("b:read", res.Text, "served on")
	c.Eq(1, fb.provisionsOn("b"), "provisions on the new home")
	c.Eq(1, fb.migrations, "the child must be told its workspace moved; migrations")
	c.Eq("a", fb.lastFrom, "NotifyMigrated from")
	c.Eq("b", fb.lastTo, "NotifyMigrated to")
	fb.mu.Lock()
	labels := append([]string(nil), fb.homeLabels...)
	fb.mu.Unlock()
	c.EqDeep([]string{"c1|b"}, labels, "the durable home label must follow the migration, or a resumed child disagrees")
	execID, _, bound := b.Current()
	c.True(bound, "bound")
	c.Eq("b", execID, "home")

	// Named: the same lost executor is a connection failure, not a migration.
	fb2 := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb2.reachableTo("b")
	fb2.mode = "ephemeral"
	fb2.homeLabel = "a"
	b2 := newBoundExecutor("c2", fb2)
	_, err = b2.Execute(namedCall("a"), "read", nil)
	c.Error(err, "a named call to a departed executor must fail")
	c.Eq("executor a is not connected; calls to it fail until it reconnects", err.Error(), "error text")
	c.Eq(0, fb2.provisions, "a named call never migrates; got")
	c.Eq(0, fb2.migrations, "migrations")
}

// A kill is resolved on the executor that served it, and only there: "gone"
// means an executor restart lost the job, so pruning the wrong executor's set
// leaves the real one free to report the self-killed job as lost.
func TestBoundExecutorKillJobForgetsOnServingExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	b := newBoundExecutor("c1", fb)

	handle, err := b.StartJob(namedCall("b"), "make check")
	c.NoError(err, "StartJob")
	c.Eq("b:job", handle, "handle")
	c.NoError(b.KillJob(namedCall("b"), handle), "KillJob")
	fb.mu.Lock()
	forgot := append([]string(nil), fb.forgotRunning...)
	fb.mu.Unlock()
	c.EqDeep([]string{"b|" + handle}, forgot, "the serving executor's running set is the one pruned")
}

// The spawn path knows the home executor before any call exists (the child's row
// does not exist yet, so its label cannot be read back). setHome is how it says
// so, and the binding it names must then behave as home: the cwd applies and the
// labels are written.
func TestBoundExecutorSetHomeBeforeFirstCall(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	b := newBoundExecutor("c1", fb)
	b.setHome("b")

	tg := &tools.ExecutorTarget{Requested: "b"}
	if _, err := b.Execute(tools.WithExecutorTarget(context.Background(), tg), "read", nil); err != nil {
		t.Fatalf("named call on the home executor: %v", err)
	}
	c.EqDeep([]bool{true}, fb.useCwdFor("b"), "the home binding provisions with the child's cwd")
	fb.mu.Lock()
	noted := append([]string(nil), fb.noted...)
	fb.mu.Unlock()
	c.Len(noted, 1, "the home binding writes the labels; noted")
	c.StrContains(noted[0], "b/", "noted value")
	c.Eq("", tg.Note, "home gets no note")

	if _, err := b.Execute(namedCall("a"), "read", nil); err != nil {
		t.Fatalf("named call on a: %v", err)
	}
	c.EqDeep([]bool{false}, fb.useCwdFor("a"), "a non-home binding starts at that machine's root")
}

// The project tier belongs to the home workspace. With no home there is nothing
// to read, and answering from another machine's root would feed the child the
// wrong project's instructions.
func TestBoundExecutorContextFromHomeOnly(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	b := newBoundExecutor("c1", fb)

	_, err := b.ProjectContext(context.Background())
	c.Error(err, "no home executor means no project context")
	c.Eq("this agent has no home executor", err.Error(), "error text")
	c.Eq(0, fb.provisions, "no provisioning is a side effect of asking; got")
}

// A child that called tools on two machines has workspaces the LABELS never
// name — the labels carry the home pair only — so teardown must release the
// rest from the binding itself. Without that, every such workspace (and its
// retained job output) is stranded on a live executor for the daemon's life.
func TestExitReleasesEveryBinding(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)

	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	be := newBoundExecutor(childID, fb)
	be.setHome("a")
	if _, err := be.clientFor(context.Background(), "a"); err != nil {
		t.Fatalf("bind home: %v", err)
	}
	fb.reachableTo("a", "b")
	if _, err := be.Execute(namedCall("b"), "read", nil); err != nil {
		t.Fatalf("call on b: %v", err)
	}
	bindings := be.Bindings()
	wsHome, wsOther := bindings["a"], bindings["b"]
	ck := assert.NewAborting(t)
	ck.False(wsHome == "" || wsOther == "", "the fixture did not create both bindings: %v", bindings)

	ctrl.noteBoundExecutor(childID, be)
	if _, err := ctrl.st.SetLabels(childID, map[string]string{
		"rafiki/workspace": wsHome, "rafiki/executor": "a",
	}, nil); err != nil {
		t.Fatalf("set labels: %v", err)
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	// The release runs on its own goroutine, inside teardown's deadline.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fb.mu.Lock()
		released := fb.released[wsOther]
		homeByBinding := fb.released[wsHome]
		fb.mu.Unlock()
		if released {
			ck.False(homeByBinding, "the labelled home workspace belongs to the label-driven release, "+
				"not to the binding sweep; released via the binder too")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace %q on the second executor was never released; bindings were %v", wsOther, bindings)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An UNNAMED SPAWN keeps the pre-multi-executor behaviour: when per-call
// resolution refuses (two machines reachable and none named), the spawn still
// binds to selection's single pick. The refusal is the agent's TOOL CALL's
// answer; refusing at spawn is a decision spawn-time home resolution owns, and
// until it lands an unnamed spawn must work on a multi-executor daemon exactly
// as it did when a child could reach only one machine.
func TestBoundExecutorUnnamedSpawnFallsBackToSelection(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	b := newBoundExecutor("c1", fb)

	c.NoError(b.bindImplicit(context.Background()), "an unnamed spawn on a multi-executor daemon must still bind")
	execID, _, bound := b.Current()
	c.True(bound, "bound")
	c.Eq("a", execID, "selection's single pick")
	c.EqDeep([]bool{true}, fb.useCwdFor("a"), "the picked machine is the child's workspace, so its cwd applies")
	fb.mu.Lock()
	noted := append([]string(nil), fb.noted...)
	fb.mu.Unlock()
	c.Len(noted, 1, "and it owns the labels; noted")

	// When selection has nothing to give, ITS refusal is the one the spawn
	// reports — never the per-call ambiguity message.
	fb2 := newFakeBinder()
	fb2.chooseErr = errors.New(`spawn refused: no executor satisfies ""`)
	b2 := newBoundExecutor("c2", fb2)
	err := b2.bindImplicit(context.Background())
	c.Error(err, "no executor at all must still refuse the bind")
	c.Eq(`spawn refused: no executor satisfies ""`, err.Error(), "ChooseFor's refusal text")
}

// stale returns the home binding's key, for tests that then simulate a failure
// against it. Test-only: no production path asks a binding where it is.
func (b *boundExecutor) stale() (execID, wsID string) {
	home := b.homeExecutor()
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd := b.bindings[home]; bd != nil {
		return home, bd.wsID
	}
	return home, ""
}

// --- the review's gaps ------------------------------------------------------

// gatedExec fails every Execute after a per-CLASS gate opens, classifying each
// call the way the registry wrapper does: named when the call carries an
// executor argument, implicit otherwise. It is how a test can hold one class of
// caller at the call site while another is released.
type gatedExec struct {
	mu         sync.Mutex
	gates      map[string]chan struct{}
	calls      map[string]int
	dispatched chan string
}

func newGatedExec() *gatedExec {
	return &gatedExec{
		gates:      map[string]chan struct{}{"implicit": make(chan struct{}), "named": make(chan struct{})},
		calls:      map[string]int{},
		dispatched: make(chan string, 8),
	}
}

// permit opens one class's gate. Callers already parked proceed; later callers
// of that class fail immediately instead of parking again.
func (e *gatedExec) permit(class string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.gates[class]:
	default:
		close(e.gates[class])
	}
}

func (e *gatedExec) callsIn(class string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[class]
}

func (e *gatedExec) Execute(ctx context.Context, _ string, _ json.RawMessage) (toolmeta.Result, error) {
	class := "implicit"
	if t := tools.ExecutorTargetFrom(ctx); t != nil && t.Requested != "" {
		class = "named"
	}
	e.mu.Lock()
	e.calls[class]++
	gate := e.gates[class]
	e.mu.Unlock()
	e.dispatched <- class
	<-gate
	return toolmeta.Result{}, fmt.Errorf("gone: %w", execpool.ErrExecutorGone)
}

func (e *gatedExec) StartJob(context.Context, string) (string, error) { return "job", nil }
func (e *gatedExec) JobOutput(context.Context, string, int64) (tools.JobSnapshot, error) {
	return tools.JobSnapshot{Found: true}, nil
}
func (e *gatedExec) KillJob(context.Context, string) error { return nil }
func (e *gatedExec) Ping(context.Context) error            { return nil }

// A NAMED call is never retried on another executor, even when a concurrent
// IMPLICIT caller is the one whose recovery migrated the child's home.
//
// The named caller waits on the same binding's recovery, so it is handed the
// implicit caller's `ok` — and a retry that followed home would run the model's
// call on a machine it never named, which is precisely what "a named call never
// migrates" forbids.
func TestBoundExecutorNamedCallNeverRetriedElsewhere(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	gated := newGatedExec()
	fb.mu.Lock()
	fb.clientByExec["a"] = gated
	fb.mu.Unlock()

	be := newBoundExecutor("c1", fb)
	c.NoError(be.bindImplicit(context.Background()), "bind home a")

	// Hold b's provision open so the migration is provably in flight when the
	// named caller's failure arrives.
	migrating := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(execID string) {
		if execID != "b" {
			return
		}
		once.Do(func() { close(migrating) })
		<-release
	}
	fb.mu.Unlock()

	type outcome struct {
		res toolmeta.Result
		err error
	}
	namedOut := make(chan outcome, 1)
	go func() {
		res, err := be.Execute(namedCall("a"), "read", nil)
		namedOut <- outcome{res, err}
	}()
	implicitOut := make(chan outcome, 1)
	go func() {
		res, err := be.Execute(context.Background(), "read", nil)
		implicitOut <- outcome{res, err}
	}()
	// Both calls must be IN FLIGHT on the binding before a departs: a call
	// dispatched afterwards would be refused by resolution instead, which is a
	// different (and already tested) path.
	for range 2 {
		select {
		case <-gated.dispatched:
		case <-time.After(2 * time.Second):
			t.Fatal("a call never reached the executor")
		}
	}

	// a departs while the two calls are in flight; b is the only machine left.
	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	// The implicit caller fails first, leads the recovery, and blocks migrating
	// to b; the named caller then fails and becomes a waiter.
	gated.permit("implicit")
	<-migrating
	gated.permit("named")
	close(release)

	// Both outcomes first: the implicit caller's retry may still be in flight,
	// and its call on b is part of what the count below means.
	named := <-namedOut
	implicit := <-implicitOut

	c.Error(named.err, "a named call must not be retried on another executor")
	c.ErrorIs(named.err, execpool.ErrExecutorGone, "the named call's own failure must surface, got")
	c.Eq(1, gated.callsIn("named"), "the named call ran exactly once, on the executor it named; got %d")

	bExec := fb.execs["b"]
	bExec.mu.Lock()
	bCalls := bExec.calls
	bExec.mu.Unlock()
	// Exactly one call reaches b: the implicit caller's retry. A named retry
	// that followed home would make it two.
	c.Eq(1, bCalls, "the named call must never run on the executor the implicit caller migrated to; b ran %d time(s)")

	c.NoError(implicit.err, "the implicit call follows its work to the new home")
	c.Eq("b:read", implicit.res.Text, "served on")
	c.Eq(1, fb.provisionsOn("b"), "provisions on the new home")
}

// The home fetches obey the SAME live set as every tool call: a home the child
// is no longer admitted to is not a provision target, however it became stale.
func TestBoundExecutorHomeOutsideReachableIsNotProvisioned(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.homeLabel = "a" // the durable home, from a grant the child has since lost
	fb.reachableTo("b")
	be := newBoundExecutor("c1", fb)

	_, err := be.ProjectContext(context.Background())
	c.Error(err, "a home outside the live set must refuse")
	c.StrContains(err.Error(), "not connected", "the refusal must read as a lost machine; got %q", err)
	c.Eq(0, fb.provisionsOn("a"), "no workspace may be made on an executor outside the live set; got %d")
	c.Eq(0, fb.provisions, "and none anywhere else; got %d")

	_, err = be.MaterializeSkill(context.Background(), "treehash", nil)
	c.Error(err, "the skill files are read on the same home, so the same refusal")
	c.StrContains(err.Error(), "not connected", "got %q", err)
	c.Eq(0, fb.provisionsOn("a"), "provisions on the unreachable home; got %d")
}

// A slow FIRST provision on one executor must not block the child's other
// bindings. With one binding a map-wide lock was harmless; with a map it means a
// hung executor stalls every healthy call on the child for the full RPC timeout.
func TestBoundExecutorSlowFirstBindDoesNotBlockAnotherBinding(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)

	fb.mu.Lock()
	fb.homeLabel = "a"
	fb.mu.Unlock()
	if _, err := be.ensureBinding(context.Background(), "a", true); err != nil {
		t.Fatalf("bind a: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(execID string) {
		if execID != "b" {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	}
	fb.mu.Unlock()

	go func() { _, _ = be.Execute(namedCall("b"), "read", nil) }()
	<-entered

	done := make(chan error, 1)
	go func() {
		_, err := be.Execute(namedCall("a"), "read", nil)
		done <- err
	}()
	select {
	case err := <-done:
		c.NoError(err, "a call on a healthy binding must not wait behind another executor's first provision")
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a call on a healthy binding blocked behind an unrelated first provision; the provision is holding a lock it must not")
	}
	close(release)
}

// A first provision that FAILS must leave the child without a home: a home with
// nothing behind it makes a later call report "pinned there" (or migrate) for a
// child that never bound anywhere.
func TestBoundExecutorFailedFirstProvisionLeavesNoHome(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "b"})
	fb.reachableTo("a") // reachable, but ProvisionOn("a") has no such executor
	fb.mode = "pinned"
	be := newBoundExecutor("c1", fb)

	_, err := be.Execute(context.Background(), "read", nil)
	c.Error(err, "the provision fails")
	c.Eq("", be.homeExecutor(), "a failed first provision must leave NO home; got")
	_, _, bound := be.Current()
	c.False(bound, "unbound")

	// The consequence: the next call must simply adopt the machine that is
	// there, not inherit a home it never had a workspace on.
	fb.reachableTo("b")
	res, err := be.Execute(context.Background(), "read", nil)
	c.NoError(err, "a child that never bound must adopt the reachable machine, not be pinned to a phantom home")
	c.Eq("b:read", res.Text, "served on")
	c.Eq("b", be.homeExecutor(), "home")
}

// A migration onto a machine the child already used for a NAMED call must
// RE-PROVISION that workspace: it was made at that machine's root, so promoting
// it as home would silently leave the child's cwd unapplied. And the new home is
// labelled exactly once.
func TestBoundExecutorMigrationPromotesARootWorkspace(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a", "b")
	be := newBoundExecutor("c1", fb)
	be.setHome("a")
	if _, err := be.ensureBinding(context.Background(), "a", true); err != nil {
		t.Fatalf("bind home a: %v", err)
	}

	// A named call on b makes a ROOT-level workspace there.
	if _, err := be.Execute(namedCall("b"), "read", nil); err != nil {
		t.Fatalf("named call on b: %v", err)
	}
	rootWS := be.Bindings()["b"]
	c.EqDeep([]bool{false}, fb.useCwdFor("b"), "the named call's workspace starts at b's root")
	fb.mu.Lock()
	notedBefore := len(fb.noted)
	fb.mu.Unlock()
	c.Eq(1, notedBefore, "only the home binding is labelled so far; noted")

	// a departs; the implicit call migrates home onto b.
	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()
	res, err := be.Execute(context.Background(), "read", nil)
	c.NoError(err, "an ephemeral child migrates")
	c.Eq("b:read", res.Text, "served on")
	c.EqDeep([]bool{false, true}, fb.useCwdFor("b"), "the promoted workspace must be re-provisioned WITH the child's cwd")
	c.Eq(2, fb.provisionsOn("b"), "provisions on b")

	fb.mu.Lock()
	noted := len(fb.noted)
	fb.mu.Unlock()
	c.Eq(2, noted, "the new home is labelled exactly once (the migration must not write the labels again); noted")

	deadline := time.Now().Add(5 * time.Second)
	for {
		fb.mu.Lock()
		released := fb.released[rootWS]
		fb.mu.Unlock()
		if released {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root-level workspace %q was never released after the promotion", rootWS)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Concurrent implicit callers all decide to migrate at once, and the child must
// still end up with ONE home workspace, one label write and one steer.
func TestBoundExecutorConcurrentImplicitCallersMigrateOnce(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	c.NoError(be.bindImplicit(context.Background()), "bind home a")

	fb.reachableTo("b")
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	const n = 4
	start := make(chan struct{})
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := be.Execute(context.Background(), "read", nil)
			if err != nil {
				results[i] = "ERR: " + err.Error()
				return
			}
			results[i] = res.Text
		}()
	}
	close(start)
	wg.Wait()

	want := make([]string, n)
	for i := range want {
		want[i] = "b:read"
	}
	c.EqDeep(want, results, "every implicit caller follows the child to the new home")
	c.Eq(1, fb.provisionsOn("b"), "one home workspace, however many callers raced; got %d")
	c.Eq(1, fb.migrations, "the child is told once; migrations")
	fb.mu.Lock()
	labels := append([]string(nil), fb.homeLabels...)
	fb.mu.Unlock()
	c.EqDeep([]string{"c1|b"}, labels, "one durable home label; got")
}

// A dead executor's workspace release must not starve the live ones: teardown
// releases them concurrently, each on its own deadline. Sequentially they shared
// one budget in map order, so one hung machine could eat it all.
func TestReleaseOthersReleasesConcurrently(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"}, &fakeExec{id: "c"})
	be := newBoundExecutor("c1", fb)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := be.ensureBinding(context.Background(), id, false); err != nil {
			t.Fatalf("bind %s: %v", id, err)
		}
	}

	entered := make(chan string, 3)
	release := make(chan struct{})
	fb.mu.Lock()
	fb.onRelease = func(execID string) {
		entered <- execID
		<-release
	}
	fb.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		be.ReleaseOthers(context.Background(), "", "")
	}()

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(2 * time.Second):
			close(release)
			<-done
			t.Fatalf("only %d of 3 releases started within the deadline; teardown is serialising them behind each other", len(seen))
		}
	}
	close(release)
	<-done
	c.Len(seen, 3, "released executors")
}
