package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
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

	// reachNext, when non-empty, is a queue of reachable sets popped ONE PER
	// Reachable call. It is how a test gives two callers different views of the
	// live set at the same moment -- what a concurrent migration to two
	// different targets needs.
	reachNext [][]string

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

	// labels is what NoteBinding writes and ClearBinding removes, so a test can
	// assert on the child's durable labels rather than on the calls that made
	// them.
	labels map[string]string
	clears int
	undos  int

	// provisioned records every workspace id ProvisionOn ever handed out, in call
	// order, so a test can assert that each one was either released or is still
	// named by a live binding — nothing leaked.
	provisioned []string

	// onNoteBinding, when set, runs synchronously inside NoteBinding -- with the
	// fake's lock released, AFTER the labels are written. It is the window the
	// exit sweep can land in: a test drives teardown from here.
	onNoteBinding func()

	// beforeNoteBinding, when set, runs synchronously inside NoteBinding -- with
	// the fake's lock released, BEFORE the labels are written. It is the window
	// between a home binding's workspace publication and its label write, where a
	// loss can land after the publish and before the labels name the workspace.
	beforeNoteBinding func()

	// onWorkspaceMode, when set, runs synchronously inside WorkspaceMode with
	// the fake's lock released; execID is the executor whose mode is asked for.
	// It is the last binder call before a migration starts, so a test uses it to
	// pin which home a racing caller has already read.
	onWorkspaceMode func(execID string)

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

	// onNotifyMigrated, when set, runs synchronously inside NotifyMigrated -- with
	// f.mu NOT held -- so a test can hold a migration open AFTER it claimed the
	// home and while it still holds the home-transition slot. That is the window
	// an ordinary first provision writes its labels in without the slot.
	onNotifyMigrated func()

	// onProvision, when set, runs synchronously inside ProvisionOn -- with
	// f.mu NOT held -- before it does anything else. It lets a test hold a
	// Provision RPC open to prove a caller (recover) isn't holding
	// boundExecutor's lock across it.
	onProvision func(execID string)

	// onProvisionCtx is onProvision with the CALL's context, for tests that
	// need the provision to observe cancellation.
	onProvisionCtx func(ctx context.Context, execID string)

	// ensureCalls records EnsureConnected calls in order, and ensureErr is what
	// it answers. ensureFlipsLive makes a successful call bring the executor up,
	// the way a real relaunch does.
	ensureCalls     []string
	ensureErr       error
	ensureFlipsLive bool

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
		labels:       map[string]string{},
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
	if len(f.reachNext) > 0 {
		f.reachable = f.reachNext[0]
		f.reachNext = f.reachNext[1:]
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

func (f *fakeBinder) ProvisionOn(ctx context.Context, id string, useCwd bool) (string, string, tools.ExecutorClient, error) {
	f.mu.Lock()
	hook, hookCtx := f.onProvision, f.onProvisionCtx
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if hookCtx != nil {
		hookCtx(ctx, id)
	}
	// A real Provision RPC fails when its call is cancelled; the fake does too,
	// so a test can drive the cancelled-leader shape.
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
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
		ws := f.mintLocked(id)
		return ws, workdir, f.clientOverride, nil
	}
	if override, ok := f.clientByExec[id]; ok {
		ws := f.mintLocked(id)
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
	ws := f.mintLocked(id)
	return ws, workdir, e, nil
}

// mintLocked records the workspace id this provision hands out. Caller holds
// f.mu.
func (f *fakeBinder) mintLocked(id string) string {
	ws := fmt.Sprintf("ws-%s-%d", id, f.provisions)
	f.lastWorkspace = ws
	f.provisioned = append(f.provisioned, ws)
	return ws
}

// waitForReleases polls until the fake has recorded at least n releases, or the
// deadline passes. The releases run on their own goroutines (each on its own
// deadline by contract), so a test cannot read them the instant a caller
// returns.
func waitForReleases(t *testing.T, fb *fakeBinder, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fb.mu.Lock()
		got := fb.releasedWorkspaces()
		fb.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// releasedWorkspaces is the release record as a slice. Caller holds f.mu or
// calls it from a test goroutine after the releases settled.
func (f *fakeBinder) releasedWorkspaces() []string {
	out := make([]string, 0, len(f.released))
	for ws := range f.released {
		out = append(out, ws)
	}
	return out
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

// EnsureConnected records every call and, when ensureFlipsLive is set, behaves
// like a real relaunch: the executor becomes live (and reachable) the moment the
// fake is asked to bring it back.
func (f *fakeBinder) EnsureConnected(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, id)
	if f.ensureErr != nil {
		return f.ensureErr
	}
	if f.ensureFlipsLive {
		f.liveByID[id] = true
		if !slices.Contains(f.reachable, id) {
			f.reachable = append(f.reachable, id)
		}
	}
	return nil
}

func (f *fakeBinder) NoteBinding(_, execID, wsID string) {
	f.mu.Lock()
	before, hook := f.beforeNoteBinding, f.onNoteBinding
	f.mu.Unlock()
	if before != nil {
		before()
	}

	f.mu.Lock()
	f.noted = append(f.noted, execID+"/"+wsID)
	if f.labels == nil {
		f.labels = map[string]string{}
	}
	f.labels["rafiki/executor"] = execID
	f.labels["rafiki/workspace"] = wsID
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// ClearBinding removes what NoteBinding wrote, mirroring the store.
func (f *fakeBinder) ClearBinding(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears++
	delete(f.labels, "rafiki/executor")
	delete(f.labels, "rafiki/workspace")
}

// UndoBinding restores the labels a lost migration wrote, and only while they
// still name the pair it wrote — a blind clear would delete a live binding's
// only handle.
func (f *fakeBinder) UndoBinding(_, wroteExec, wroteWS, priorExec string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labels["rafiki/executor"] != wroteExec || f.labels["rafiki/workspace"] != wroteWS {
		return false
	}
	delete(f.labels, "rafiki/workspace")
	if priorExec == "" {
		delete(f.labels, "rafiki/executor")
	} else {
		f.labels["rafiki/executor"] = priorExec
	}
	f.undos++
	return true
}

// bindingLabels is a copy of the child's durable binding labels.
func (f *fakeBinder) bindingLabels() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.labels))
	for k, v := range f.labels {
		out[k] = v
	}
	return out
}

// WorkspaceMode returns the scripted mode for every executor. A single value
// is enough for these tests: they exercise the pinned/ephemeral decision, not
// per-executor mode variation.
func (f *fakeBinder) WorkspaceMode(execID string) string {
	f.mu.Lock()
	hook, mode := f.onWorkspaceMode, f.mode
	f.mu.Unlock()
	if hook != nil {
		hook(execID)
	}
	return mode
}

func (f *fakeBinder) NotifyMigrated(_, from, to string) {
	f.mu.Lock()
	hook := f.onNotifyMigrated
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
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

// bindTestHome binds be to execID as its HOME through the path production uses
// -- adoptHome, which provisions the workspace with the child's cwd, notes the
// labels and records the home. A spawn reaches it through `bindHome` →
// `clientFor`; a lazy first tool call through `resolve`. Test-only setup:
// bindImplicit used to stand in for it and hid the ambiguity rule behind a
// ChooseFor fallback that production never took.
func bindTestHome(t *testing.T, b *boundExecutor, execID string) {
	t.Helper()
	if err := b.adoptHome(context.Background(), execID); err != nil {
		t.Fatalf("bind home %s: %v", execID, err)
	}
}

func TestBoundExecutorConcurrentFailuresProduceOneRebind(t *testing.T) {
	c := assert.NewAborting(t)
	const n = 8
	burst := newBurstExec(n)
	fb := newFakeBinder()
	fb.clientOverride = burst
	b := newBoundExecutor("child-1", fb)

	// Bind first, so all n failures land against the SAME workspace.
	home, cerr := fb.ChooseFor("child-1")
	c.NoError(cerr, "ChooseFor")
	bindTestHome(t, b, home)
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
	home, cerr := f.ChooseFor("c1")
	c.NoError(cerr, "ChooseFor")
	bindTestHome(t, b, home)
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
	home, cerr := f.ChooseFor("c1")
	if cerr != nil {
		t.Fatal(cerr)
	}
	bindTestHome(t, b, home)
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

// Losing the executor never fails a pinned child: it is not re-bound to a
// different executor and no workspace is made on the machine that is still
// there, so the CALL fails until the pinned executor returns and is then
// re-provisioned in place.
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
	c.EqDeep([]string{"c1|a", "c1|b"}, labels,
		"the durable home label must record the adoption AND follow the migration, or a resumed child disagrees; got")
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
	bindTestHome(t, be, "a")

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
	bindTestHome(t, be, "a")

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
	c.EqDeep([]string{"c1|a", "c1|b"}, labels,
		"the adoption records its home and the migration moves it, once each; got")
}

// Two concurrent implicit callers whose live sets have flipped to DIFFERENT
// single executors both decide to migrate the SAME home away. The label write
// happens inside the PROVISION, i.e. before the home claim, so two migrations at
// once can leave `rafiki/executor`/`rafiki/workspace` naming the loser's
// workspace while `rafiki/home-executor` — what a resumed child binds by — and
// b.home name the winner. That does not self-heal: the next event may be a daemon
// restart, not another migration. Exactly ONE migration may run.
//
// The ordering here is forced rather than hoped for: caller A's provision is held
// until B has read the home A is migrating away from, and (in the world without
// the guard) B's provision is held until A's own migration has finished, so a
// second migration is always given the worst possible order.
func TestBoundExecutorConcurrentMigrationsToOneHome(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"}, &fakeExec{id: "c"})
	be := newBoundExecutor("c1", fb)
	bindTestHome(t, be, "a")

	// The two callers see a DIFFERENT single-executor live set, which is what
	// gives them different migration targets.
	fb.mu.Lock()
	fb.reachNext = [][]string{{"b"}, {"c"}}
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	aDone := make(chan struct{})
	releaseC := make(chan struct{})
	var enteredAOnce sync.Once
	fb.onProvision = func(execID string) {
		switch execID {
		case "b":
			enteredAOnce.Do(func() { close(enteredA) })
			<-releaseA
		case "c":
			// Whatever provisions here, in either world, does it only after A's
			// migration has completed: a loser's label write must be as LATE as
			// it can be, or the test would pass by accident.
			<-releaseC
		}
	}
	fb.mu.Unlock()

	aResult := make(chan string, 1)
	go func() {
		defer close(aDone)
		res, err := be.Execute(context.Background(), "read", nil)
		if err != nil {
			aResult <- "ERR: " + err.Error()
			return
		}
		aResult <- res.Text
	}()
	<-enteredA // A's migration is in flight: it holds the guard and the home is still a

	// B must have read home=a before A claims it, or it would be a SEQUENTIAL
	// migration (a legal one, and not what this test is about). WorkspaceMode is
	// the last binder call before migrateHome, so park B there until the test
	// knows its home read happened.
	bRead := make(chan struct{})
	var bReadOnce sync.Once
	fb.mu.Lock()
	fb.onWorkspaceMode = func(string) { bReadOnce.Do(func() { close(bRead) }) }
	fb.mu.Unlock()

	bResult := make(chan string, 1)
	go func() {
		res, err := be.Execute(context.Background(), "read", nil)
		if err != nil {
			bResult <- "ERR: " + err.Error()
			return
		}
		bResult <- res.Text
	}()
	<-bRead

	// A wins: it is the one already provisioning. Release it, let it claim, and
	// only then let anything else provision.
	close(releaseA)
	c.Eq("b:read", <-aResult, "the first caller must serve its call on the executor it migrated to")
	<-aDone
	close(releaseC)
	c.Eq("c:read", <-bResult, "losing the race must not fail the second caller: its call runs on the executor it resolved")

	home := be.homeExecutor()
	c.Eq("b", home, "home is the winner's executor; got")
	execID, wsID, bound := be.Current()
	c.True(bound, "the child is bound")
	c.Eq("b", execID, "Current executor")
	c.True(wsID != "", "Current workspace")

	fb.mu.Lock()
	noted := append([]string(nil), fb.noted...)
	homeLabels := append([]string(nil), fb.homeLabels...)
	migrations := fb.migrations
	fb.mu.Unlock()
	labels := fb.bindingLabels()

	c.EqDeep([]string{"c1|a", "c1|b"}, homeLabels,
		"the spawn's adoption records a, exactly ONE migration moves it to b, and no second migration writes a home the labels do not name; got")
	c.Eq(1, migrations, "the child is told its workspace moved once; migrations")
	c.Len(noted, 2, "exactly one label write for the migration (the spawn's home bind is the other); noted %v", noted)
	c.Eq("b/"+wsID, noted[len(noted)-1], "the labels must name the HOME binding's executor and workspace; noted %v", noted)
	c.Eq("b", labels["rafiki/executor"], "rafiki/executor must agree with b.home and rafiki/home-executor; labels %v", labels)
	c.Eq(wsID, labels["rafiki/workspace"], "rafiki/workspace must name the home binding's workspace; labels %v", labels)
}

// assertHomeAndLabelsAgree is the invariant the migration/loss protocol exists
// to keep: the durable labels and b.home name the SAME executor and workspace,
// or they name no workspace at all. A resume reads those labels, so a
// disagreement outlives the daemon that made it.
func assertHomeAndLabelsAgree(t *testing.T, be *boundExecutor, fb *fakeBinder) {
	t.Helper()
	home := be.homeExecutor()
	labels := fb.bindingLabels()
	wsLabel := labels["rafiki/workspace"]
	if home == "" {
		if wsLabel != "" {
			t.Fatalf("b.home names no executor, but the labels name workspace %q (labels %v): a "+
				"resume would bind a home nothing claimed", wsLabel, labels)
		}
		return
	}
	if execLabel := labels["rafiki/executor"]; execLabel != home {
		t.Fatalf("b.home is %q but rafiki/executor is %q (labels %v)", home, execLabel, labels)
	}
	if wsLabel == "" {
		t.Fatalf("b.home is %q but rafiki/workspace names nothing (labels %v)", home, labels)
	}
	if bound := be.Bindings()[home]; bound != wsLabel {
		t.Fatalf("the labels name workspace %q but the home binding on %q names %q", wsLabel, home, bound)
	}
}

// assertNoWorkspaceLeaked checks that every workspace the fake provisioned is
// either released, or still named by a live binding (which the exit sweep
// releases).
func assertNoWorkspaceLeaked(t *testing.T, be *boundExecutor, fb *fakeBinder) {
	t.Helper()
	held := make(map[string]bool, len(be.Bindings()))
	for _, ws := range be.Bindings() {
		held[ws] = true
	}
	fb.mu.Lock()
	provisioned := append([]string(nil), fb.provisioned...)
	released := fb.releasedWorkspaces()
	fb.mu.Unlock()
	for _, ws := range provisioned {
		ok := held[ws]
		for _, rel := range released {
			if rel == ws {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("workspace %q was provisioned and is neither released nor held by a binding "+
				"(provisioned %v, released %v, held %v)", ws, provisioned, released, held)
		}
	}
}

// A loss that lands in the ONE window between a migration's label write and its
// home claim must not leave the durable labels naming the migration's target
// while the daemon's home is empty: those labels are what a resume reads, so the
// disagreement survives a restart onto a home the daemon never claimed.
//
// The window is opened from inside the label write itself (fakeBinder's
// onNoteBinding), which is where a migration publishes its target's labels and
// before it claims the home. The loss cannot take the transition slot — the
// migration holds it, and the migration cannot make progress while this hook
// runs — so the loss proceeds without it and the migration loses its claim,
// which is the branch that has to roll its own write back.
func TestLostExecutorCannotClearTheHomeAMigrationHasNotClaimed(t *testing.T) {
	c := assert.NewAborting(t)

	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)
	bindTestHome(t, be, "a")
	// One reachable executor that is not the home: that is what an implicit call
	// migrates to.
	fb.reachableTo("b")
	homeWS, hadHome := be.Bindings()["a"]
	c.Require().True(hadHome, "test setup: the spawn bound a home workspace on a")

	var (
		forgetWS string
		forgot   bool
	)
	fb.mu.Lock()
	fb.onNoteBinding = func() {
		// The window: the migration has published the target's labels and has not
		// claimed the home yet.
		forgetWS, forgot = be.Forget("a")
		if forgot {
			// Releasing the forgotten workspace is the loss handler's job.
			fb.ReleaseOn(context.Background(), "a", forgetWS)
		}
	}
	fb.mu.Unlock()

	res, err := be.Execute(context.Background(), "read", nil)
	c.NoError(err, "losing the race must not fail the call: it proceeds on the executor it resolved")
	c.Eq("b:read", res.Text, "the call must run on the executor the migration resolved; got")
	c.True(forgot, "the loss must forget the home binding")
	c.Eq(homeWS, forgetWS, "and hand its workspace to the caller for release; got")

	assertHomeAndLabelsAgree(t, be, fb)
	c.Eq("", be.homeExecutor(), "the migration never claimed the home, so the child has none; got")
	_, claims := fb.bindingLabels()["rafiki/workspace"]
	c.False(claims, "and no label may keep naming the workspace it provisioned")
	fb.mu.Lock()
	undos := fb.undos
	fb.mu.Unlock()
	c.Eq(1, undos, "the lost migration must roll its own label write back; undos")

	// The rollback releases its workspace on its own goroutine (each release has
	// its own deadline by contract), so the release record is read after it lands.
	released := waitForReleases(t, fb, 2)
	c.Len(released, 2, "the loss AND the rollback must release their workspaces; released %v", released)
	assertNoWorkspaceLeaked(t, be, fb)
}

// A loss NEVER waits for the home-transition slot: Forget runs on the executor
// pool's park-sweep goroutine for every affected child in sequence, so a wait
// behind one child's provision RPC would delay every other child's teardown.
// The loss therefore proceeds WITHOUT the slot and takes the documented loser
// side: the migration it collided with finds the home cleared, loses its claim,
// and rolls its own label write back.
func TestForgetOutrunsAnInFlightHomeMigrationAndRollsItBack(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)
	bindTestHome(t, be, "a")
	fb.reachableTo("b")

	// Hold the migration inside its provision RPC: it holds the transition slot,
	// and the loss below must not wait for it.
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(id string) {
		if id != "b" {
			return
		}
		once.Do(func() { close(entered) })
		<-proceed
	}
	fb.mu.Unlock()

	type callResult struct {
		text string
		err  error
	}
	callDone := make(chan callResult, 1)
	go func() {
		res, err := be.Execute(context.Background(), "read", nil)
		callDone <- callResult{text: res.Text, err: err}
	}()
	<-entered

	forgetDone := make(chan struct{})
	go func() {
		defer close(forgetDone)
		if ws, ok := be.Forget("a"); ok && ws != "" {
			// Releasing the forgotten workspace is the loss handler's job.
			fb.ReleaseOn(context.Background(), "a", ws)
		}
	}()
	select {
	case <-forgetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Forget blocked on the home-transition slot: a loss must never wait for one")
	}
	c.Eq("", be.homeExecutor(), "the loss cleared the home while the migration provisioned; home =")

	close(proceed)
	res := <-callDone
	c.NoError(res.err, "the call must still be served")
	c.Eq("b:read", res.text, "on the executor the migration resolved; got")
	c.Eq("", be.homeExecutor(), "the migration lost its claim to the loss, so the child has no home; home =")
	assertHomeAndLabelsAgree(t, be, fb)
	fb.mu.Lock()
	migrations, undos := fb.migrations, fb.undos
	fb.mu.Unlock()
	c.Eq(0, migrations, "a migration that lost the home must not claim it or steer the child; migrations")
	c.Eq(1, undos, "it must roll its own label write back instead; undos")
	waitForReleases(t, fb, 2)
	assertNoWorkspaceLeaked(t, be, fb)
}

// An adoption is the LAZIEST home transition — it runs mid-child, on a live
// binding, the moment a call finds no home — so it is the one most likely to
// meet a loss, and it takes the same slot a migration and a loss take. A loss
// that cannot take the slot (the adoption is inside its provision RPC) forgets
// the binding the adoption published and its caller rewrites the labels; the
// adoption must then DECLINE the home rather than record one over labels that
// rewrite has already cleared.
func TestLostExecutorCannotOutrunAnAdoption(t *testing.T) {
	c := assert.NewAborting(t)

	fb := newFakeBinder(&fakeExec{id: "a"})
	be := newBoundExecutor("c1", fb)
	// No home at all — the child a loss left unbound — and one executor it can
	// reach, which the call below adopts.
	fb.reachableTo("a")

	var (
		forgetWS string
		forgot   bool
	)
	fb.mu.Lock()
	fb.onNoteBinding = func() {
		// The window: the adoption has published the labels for a, and has not
		// recorded the home yet.
		forgetWS, forgot = be.Forget("a")
		if forgot {
			// The loss handler's job: release the forgotten workspace, then stop
			// the labels claiming one.
			fb.ReleaseOn(context.Background(), "a", forgetWS)
		}
		fb.mu.Lock()
		delete(fb.labels, "rafiki/workspace")
		fb.mu.Unlock()
	}
	fb.mu.Unlock()

	// The adoption transition itself, with nothing after it: the follow-up call
	// would re-provision on the (in this fixture still reachable) executor and
	// paper over exactly the state being asserted.
	err := be.adoptHome(context.Background(), "a")
	c.NoError(err, "losing its binding to a loss is not a failure for the adoption")
	c.True(forgot, "the loss must forget the binding the adoption published")
	c.Eq("", be.homeExecutor(), "the adoption lost its binding, so it must claim NO home; home =")
	assertHomeAndLabelsAgree(t, be, fb)

	fb.mu.Lock()
	releases, homeLabels := fb.releases, append([]string(nil), fb.homeLabels...)
	fb.mu.Unlock()
	c.Eq(1, releases, "the loss releases the workspace it was handed, once — the adoption must not release it again; releases")
	c.Empty(homeLabels, "and an adoption that never claimed must record no home label; homeLabels %v", homeLabels)
	assertNoWorkspaceLeaked(t, be, fb)
}

// The mirror of the migration case: a loss does not wait for an ADOPTION either.
// It forgets the placeholder the adoption published, so the adoption's own
// publish is superseded and no home is ever recorded — the decline by another
// route. The labels stay empty, so a resume has nothing to bind.
func TestForgetOutrunsAnInFlightAdoption(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	be := newBoundExecutor("c1", fb)
	fb.reachableTo("a")

	// Hold the adoption inside its provision RPC: the placeholder exists and has
	// no workspace yet.
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(string) {
		once.Do(func() { close(entered) })
		<-proceed
	}
	fb.mu.Unlock()

	type callResult struct {
		text string
		err  error
	}
	callDone := make(chan callResult, 1)
	go func() {
		res, err := be.Execute(context.Background(), "read", nil)
		callDone <- callResult{text: res.Text, err: err}
	}()
	<-entered

	forgetDone := make(chan struct{})
	go func() {
		defer close(forgetDone)
		if ws, ok := be.Forget("a"); ok && ws != "" {
			fb.ReleaseOn(context.Background(), "a", ws)
		}
	}()
	select {
	case <-forgetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Forget blocked on the home-transition slot: a loss must never wait for one")
	}
	c.Eq("", be.homeExecutor(), "the loss leaves the child with no home; home =")

	close(proceed)
	r := <-callDone
	c.ErrorIs(r.err, errBindingSuperseded,
		"the adoption's publish was superseded by the loss, so the call reports it; got")
	c.Eq("", be.homeExecutor(), "no home may be claimed for a binding the loss forgot; home =")
	assertHomeAndLabelsAgree(t, be, fb)
	fb.mu.Lock()
	homeLabels := append([]string(nil), fb.homeLabels...)
	fb.mu.Unlock()
	c.Empty(homeLabels, "an adoption whose binding was forgotten must record no home label; homeLabels %v", homeLabels)
	waitForReleases(t, fb, 1)
	assertNoWorkspaceLeaked(t, be, fb)
}

// A home that moved off the lost machine BEFORE the loss was handled owns the
// labels now: pass 1 skips the child (rafiki/executor names the new home) and
// pass 2 forgets the leftover binding on the dead machine. The labels must be
// left alone — clearing rafiki/workspace would delete the LIVE new home's only
// label and leave rafiki/executor naming neither machine nor the home a resume
// binds. (It lives in this file, not workspace_wiring_test.go, so the loss
// protocol and its tests stay one change.)
func TestExecutorLostLeavesTheLabelsOfAHomeThatMoved(t *testing.T) {
	c := assert.NewAborting(t)
	pool := &fakePool{}
	ctrl, clk, _ := lostFixture(t, pool, row(lostExecID, "build-1"))

	fb := newFakeBinder(&fakeExec{id: lostExecID}, &fakeExec{id: otherExecID})
	be := newBoundExecutor("c_race", fb)
	be.setHome(lostExecID)
	if _, err := be.ensureBinding(context.Background(), lostExecID, true); err != nil {
		t.Fatalf("home binding on the lost executor: %v", err)
	}
	lostWS := be.Bindings()[lostExecID]
	ctrl.noteBoundExecutor("c_race", be)
	seedChildOn(ctrl, "c_race", lostExecID, lostWS, "ephemeral")

	// The migration's label write, mirrored into the CHILD'S ROW the way the
	// controller's binder does it: this test asserts on the durable labels the
	// loss handler would clobber, not on the fake's own copy.
	fb.mu.Lock()
	fb.onNoteBinding = func() {
		fb.mu.Lock()
		execID, wsID := fb.labels["rafiki/executor"], fb.labels["rafiki/workspace"]
		fb.mu.Unlock()
		_, _ = ctrl.st.SetLabels("c_race", map[string]string{
			"rafiki/executor":       execID,
			"rafiki/workspace":      wsID,
			"rafiki/workspace-mode": "ephemeral",
		}, nil)
	}
	fb.mu.Unlock()

	// The home moves to the one reachable executor and is CLAIMED before the
	// loss is handled.
	fb.reachableTo(otherExecID)
	if _, err := be.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("the call that migrates must be served: %v", err)
	}
	c.Eq(otherExecID, be.homeExecutor(), "the migration claimed the home; home =")

	ctrl.HandleExecutorLost(lostExecID)

	snap, ok := ctrl.st.Get("c_race")
	c.Require().True(ok, "the child's row survives")
	c.Eq(otherExecID, snap.Labels["rafiki/executor"], "the labels must still name the home the migration established; =")
	c.Eq(be.Bindings()[otherExecID], snap.Labels["rafiki/workspace"],
		"and the workspace that home binding holds — a loss that clears it leaves a live workspace unnamed "+
			"and unreleasable; =")
	_, unbound := snap.Labels["rafiki/executor-state"]
	c.False(unbound, "a child whose home moved is not unbound")
	c.EqDeep([]wsRelease{{executorID: lostExecID, workspaceID: lostWS}}, pool.releases(),
		"the workspace on the lost machine is still released, exactly once; got")

	clk.Advance(6 * time.Second)
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

// --- the re-review's defects (N1–N5) ---------------------------------------

// A provision still in flight when the child's bindings are swept must not
// leave its workspace behind: the exit snapshot cannot name it, and nothing
// else will ever release it. The same holds for a forgotten binding.
func TestProvisionInFlightWhenReleasedIsNotLeaked(t *testing.T) {
	c := assert.NewAborting(t)

	// Exit during an in-flight provision.
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	fb.mu.Unlock()

	provisioned := make(chan error, 1)
	go func() {
		_, err := be.ensureBinding(context.Background(), "a", false)
		provisioned <- err
	}()
	<-entered

	// The child exits: the sweep runs while the workspace is still being made.
	be.ReleaseOthers(context.Background(), "", "")
	close(release)

	err := <-provisioned
	c.Error(err, "a provision racing the exit must not hand back a binding")
	c.ErrorIs(err, errChildExited, "the caller must be told the child is gone, got")
	leftovers := waitForReleases(t, fb, 1)
	c.Len(leftovers, 1, "the workspace made after the sweep must be released; released %v", leftovers)
	_, _, bound := be.Current()
	c.False(bound, "a closed executor reports no binding")
	c.Empty(be.Bindings(), "and the dead placeholder must be gone from the map, not left claiming a binding; got %v", be.Bindings())

	// A Forget during an in-flight provision leaks the same way.
	fb2 := newFakeBinder(&fakeExec{id: "a"})
	fb2.reachableTo("a")
	be2 := newBoundExecutor("c2", fb2)
	entered2 := make(chan struct{})
	release2 := make(chan struct{})
	var once2 sync.Once
	fb2.mu.Lock()
	fb2.onProvision = func(string) {
		once2.Do(func() { close(entered2) })
		<-release2
	}
	fb2.mu.Unlock()

	provisioned2 := make(chan error, 1)
	go func() {
		_, perr := be2.ensureBinding(context.Background(), "a", false)
		provisioned2 <- perr
	}()
	<-entered2
	be2.Forget("a") // the Task 3.2 shape: forget first, release by the caller
	close(release2)

	forgottenErr := <-provisioned2
	c.Error(forgottenErr, "a provision whose binding was forgotten must not be published")
	c.ErrorIs(forgottenErr, errBindingSuperseded, "a forgotten binding gets its own sentence, not the child-exit one; got")
	released2 := waitForReleases(t, fb2, 1)
	c.Len(released2, 1, "the forgotten binding's new workspace must be released by the provisioner; released %v", released2)
}

// Promotion PROVISIONS the home workspace before it releases the root-level one.
// A failed re-provision must therefore keep the workspace the agent was using —
// its files and any retained job output — and must leave no home.
func TestBoundExecutorFailedPromotionKeepsTheOldWorkspace(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	fb.reachableTo("a", "b")
	be := newBoundExecutor("c1", fb)
	if _, err := be.ensureBinding(context.Background(), "a", false); err != nil {
		t.Fatalf("root binding on a: %v", err)
	}
	rootWS := be.Bindings()["a"]

	// Every later provision on a fails: the home re-provision cannot happen.
	fb.mu.Lock()
	delete(fb.execs, "a") // ProvisionOn("a") now answers "no such executor"
	fb.mu.Unlock()

	err := be.adoptHome(context.Background(), "a")
	c.Error(err, "the promotion's provision fails")
	c.Eq("", be.homeExecutor(), "a failed promotion must leave NO home; got")
	fb.mu.Lock()
	released := fb.released[rootWS]
	fb.mu.Unlock()
	c.False(released, "the root workspace was destroyed before its replacement existed: %v", be.Bindings())
	bindings := be.Bindings()
	c.Eq(rootWS, bindings["a"], "the old binding must still serve the executor; bindings %v", bindings)
}

// "bound" promises a workspace exists: a home placeholder whose provision has
// not finished reports the executor it is coming up on, but bound=false.
func TestBoundExecutorUnfinishedHomePlaceholderIsNotBound(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	// A resumed child: its home is already known (from rafiki/home-executor)
	// while the home workspace is still being made.
	fb.homeLabel = "a"
	be := newBoundExecutor("c1", fb)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	fb.mu.Unlock()

	go func() { _, _ = be.clientFor(context.Background(), "a") }()
	<-entered

	execID, wsID, bound := be.Current()
	c.False(bound, "a home placeholder has no workspace yet, so it is not bound")
	c.Eq("", wsID, "workspace id")
	c.Eq("a", execID, "the executor it is coming up on is still reportable")

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ws, nowBound := be.Current()
		if nowBound {
			c.NotEq("", ws, "a bound executor must name its workspace")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the provision never published its binding")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A recovery must not write its rebuilt workspace into a binding it no longer
// owns: a promotion can replace the binding while the re-provision is in
// flight, and writing into the replacement would orphan one workspace (the
// rebuild's or the promotion's own).
func TestReprovisionInPlaceDoesNotWriteIntoAReplacement(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	be.setHome("a")
	if _, err := be.ensureBinding(context.Background(), "a", true); err != nil {
		t.Fatalf("home binding: %v", err)
	}
	// Make it look like a root-level binding about to be promoted.
	be.mu.Lock()
	be.bindings["a"].cwd = false
	be.mu.Unlock()

	// The re-provision is held open; the promotion replaces the binding first.
	reprovisioning := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(execID string) {
		if execID != "a" {
			return
		}
		once.Do(func() { close(reprovisioning) })
		<-release
	}
	fb.mu.Unlock()

	recovered := make(chan bool, 1)
	go func() {
		execID, wsID := "a", ""
		be.mu.Lock()
		bd := be.bindings["a"]
		wsID = bd.wsID
		be.mu.Unlock()
		recovered <- be.reprovisionInPlace(context.Background(), bd, execID, wsID)
	}()
	<-reprovisioning

	// The promotion swaps the binding out from under the recovery: the map now
	// holds a NEW placeholder, not the binding this recovery began on.
	be.mu.Lock()
	be.bindings["a"] = &binding{ready: make(chan struct{})}
	be.mu.Unlock()

	close(release)
	c.False(<-recovered, "a rebuild that lost its binding must not report success")

	// The workspace the rebuild made must not be handed to the new binding.
	released := waitForReleases(t, fb, 1)
	c.Len(released, 1, "the orphaned rebuild must release its own workspace; released %v", released)
}

// A provision that fails because its own call was cancelled leaves NOTHING
// behind: the placeholder is dropped with its error, so the next call provisions
// afresh rather than waiting on a corpse (N2's invariant).
//
// The "a waiter inherits the leader's failure" half is deliberately NOT asserted:
// whether a second caller waits on the leader's placeholder or becomes the new
// provisioner is a scheduling detail, so a test for it is either flaky or needs a
// hook inside ensureBinding. What always holds — and what matters — is that a
// cancelled provision wedges nothing and leaks nothing.
func TestBoundExecutorCancelledProvisionLeavesNothingBehind(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)

	entered := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvisionCtx = func(ctx context.Context, execID string) {
		if execID != "a" {
			return
		}
		once.Do(func() { close(entered) })
		<-ctx.Done()
	}
	fb.mu.Unlock()

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := be.Execute(leaderCtx, "read", nil)
		leaderErr <- err
	}()
	<-entered
	cancel()
	c.ErrorIs(<-leaderErr, context.Canceled, "the leader's own cancellation, got")

	// Nothing is wedged, and nothing was provisioned behind the failure.
	fb.mu.Lock()
	fb.onProvisionCtx = nil
	fb.mu.Unlock()
	if _, err := be.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("the next call must provision afresh: %v", err)
	}
	c.Eq(1, fb.provisionsOn("a"), "one successful provision on a (the cancelled one never reached the executor)")
	fb.mu.Lock()
	released := fb.releasedWorkspaces()
	fb.mu.Unlock()
	c.Empty(released, "a cancelled provision leaves no workspace to release; released %v", released)
}

// A re-provision IN PLACE lands after the exit sweep the same way a first
// provision does: a liveness failure on a LIVE executor re-provisions, and the
// child can exit while that RPC is in flight. The rebuilt workspace must then be
// released like any other workspace made after the sweep — and the gone child
// must not be labelled.
func TestReprovisionInPlaceDuringExitIsNotLeaked(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	be.setHome("a")
	if _, err := be.ensureBinding(context.Background(), "a", true); err != nil {
		t.Fatalf("home bind: %v", err)
	}
	be.mu.Lock()
	bd, wsID := be.bindings["a"], be.bindings["a"].wsID
	labelsBefore := len(fb.noted)
	be.mu.Unlock()

	// Hold the RE-provision open (the first provision already happened).
	reprovisioning := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.mu.Lock()
	fb.onProvision = func(string) {
		once.Do(func() { close(reprovisioning) })
		<-release
	}
	fb.mu.Unlock()

	recovered := make(chan bool, 1)
	go func() {
		recovered <- be.reprovisionInPlace(context.Background(), bd, "a", wsID)
	}()
	<-reprovisioning

	// The child exits while the re-provision is in flight.
	be.ReleaseOthers(context.Background(), "", "")
	close(release)

	c.False(<-recovered, "a re-provision that lands after the sweep must report failure")
	// The sweep's release is recorded synchronously (ReleaseOthers waits on it),
	// while the re-provision's rides the publish refusal's releaseAsync, so it can
	// land after the first. Wait for BOTH; the assertion below is the guard.
	released := waitForReleases(t, fb, 2)
	c.Len(released, 2, "the sweep releases the labelled workspace and the re-provision releases its own; released %v", released)
	fb.mu.Lock()
	labelsAfter := len(fb.noted)
	fb.mu.Unlock()
	c.Eq(labelsBefore, labelsAfter, "a gone child must not be labelled by a re-provision that landed too late")
	be.mu.Lock()
	wsAfter := bd.wsID
	be.mu.Unlock()
	c.Eq(wsID, wsAfter, "the rebuilt workspace must not be installed into the binding; got")
}

// The label write is store I/O AFTER the publication guard, so an exit can land
// between the two: the sweep releases the workspace, and the write labels a child
// that is gone with a workspace nothing holds any more. The labels must be
// removed again, not left naming a released workspace — a resumed child and the
// next teardown both read them.
func TestNoteHomeBindingRemovedWhenTheChildExitsMidWrite(t *testing.T) {
	c := assert.NewAborting(t)

	// The ordinary case: nothing exits, so the labels stay.
	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	if _, err := be.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("implicit call: %v", err)
	}
	kept := fb.bindingLabels()
	c.Eq("a", kept["rafiki/executor"], "a live child keeps its binding labels; rafiki/executor")
	c.True(kept["rafiki/workspace"] != "", "a live child keeps its workspace label; labels %v", kept)
	fb.mu.Lock()
	liveClears := fb.clears
	fb.mu.Unlock()
	c.Eq(0, liveClears, "nothing exited, so nothing to remove; clears")

	// Now the exit sweep lands INSIDE the label write, with the fake's lock
	// released exactly as the store's would be.
	fb2 := newFakeBinder(&fakeExec{id: "a"})
	fb2.reachableTo("a")
	be2 := newBoundExecutor("c1", fb2)
	var once sync.Once
	fb2.onNoteBinding = func() {
		once.Do(func() { be2.ReleaseOthers(context.Background(), "", "") })
	}

	// The call's own outcome is not this test's subject: the sweep has already
	// made the child exited, so where the call lands is teardown's business.
	_, _ = be2.Execute(context.Background(), "read", nil)

	stale := fb2.bindingLabels()
	c.Len(stale, 0, "a label write that lost a race with the exit must be undone, not left naming a released workspace; got %v", stale)
	fb2.mu.Lock()
	noted, clears := len(fb2.noted), fb2.clears
	fb2.mu.Unlock()
	c.Eq(1, clears, "removed exactly once; clears")
	c.Eq(1, noted, "the write did happen — this is a compensation, not a skipped write; noted")
}

// A re-provision of the HOME binding REPLACES the workspace the child's labels
// name, so it is a home transition like a migration: it takes the slot, and a
// loss that cannot take it still proceeds without it (it never waits), so the
// label write must undo itself rather than leave the durable labels naming a
// workspace the loss already released while b.home names nothing.
//
// This is the path the earlier three fixes left open: reprovisionInPlace wrote
// the home pair outside the slot, and noteHomeBinding compensated only a child
// EXIT (b.closed), not a Forget. A resume reads those labels, so the disagreement
// outlived the daemon.
func TestReprovisionOfTheHomeBindingRacingForgetNamesNothing(t *testing.T) {
	c := assert.NewAborting(t)

	fb := newFakeBinder(&fakeExec{id: "a"})
	fb.reachableTo("a")
	be := newBoundExecutor("c1", fb)
	be.setHome("a")
	if _, err := be.ensureBinding(context.Background(), "a", true); err != nil {
		t.Fatalf("home binding: %v", err)
	}
	be.mu.Lock()
	bd, homeWS := be.bindings["a"], be.bindings["a"].wsID
	be.mu.Unlock()
	c.NotEq("", homeWS, "test setup: the home binding names a workspace")

	var (
		forgotWS string
		forgot   bool
	)
	// The loss lands between the re-provision's publication and its label write:
	// the last moment the binding still exists and the labels do not yet name the
	// workspace this re-provision made.
	fb.mu.Lock()
	fb.beforeNoteBinding = func() {
		forgotWS, forgot = be.Forget("a")
		if !forgot {
			return
		}
		// HandleExecutorLost's half of a loss: release the workspace it was handed,
		// then stop the labels claiming one.
		fb.ReleaseOn(context.Background(), "a", forgotWS)
		fb.mu.Lock()
		delete(fb.labels, "rafiki/workspace")
		fb.mu.Unlock()
	}
	fb.mu.Unlock()

	be.reprovisionInPlace(context.Background(), bd, "a", homeWS)

	c.True(forgot, "the loss must forget the home binding the re-provision had published")
	c.Eq("", be.homeExecutor(), "the loss wins the home, so the child has none; home =")
	labels := fb.bindingLabels()
	c.Eq("", labels["rafiki/workspace"],
		"the label write that landed after the loss must be undone, not left naming a workspace the "+
			"loss released: a resume would bind it; labels %v", labels)
	fb.mu.Lock()
	undos := fb.undos
	fb.mu.Unlock()
	c.Eq(1, undos, "exactly one compensation — the write did happen and was then taken back; undos")
	assertHomeAndLabelsAgree(t, be, fb)
	assertNoWorkspaceLeaked(t, be, fb)
}

// An ORDINARY home first provision writes its labels with the NON-BLOCKING policy
// (homeWaitNone): it must never block for the home-transition slot, because a
// transition holder can be waiting on the placeholder this provision published.
// So it can write its pair while another transition holds the slot — and be
// overtaken by the home having MOVED to another executor by the time the write
// returns.
//
// A resumed child has HOME a and no binding; a call provisions a as the home
// binding while a migration moves the home a -> b and claims it. The provision's
// write then lands after that claim, and its binding is still the live one on a,
// so nothing about the binding itself says it lost: only the home does. The
// durable pair must end up naming the MIGRATION's executor — anything else is the
// disagreement a resume reads, and no next call is guaranteed before a restart.
func TestFirstHomeProvisionRacingMigrationLeavesTheMigrationExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)
	be.setHome("a")

	// Hold the first provision inside its Provision RPC: it has published its
	// placeholder but has not tried to take the slot yet.
	provisioning := make(chan struct{})
	releaseProvision := make(chan struct{})
	var provisionOnce sync.Once
	fb.mu.Lock()
	fb.onProvision = func(id string) {
		if id != "a" {
			return
		}
		provisionOnce.Do(func() { close(provisioning) })
		<-releaseProvision
	}
	fb.mu.Unlock()

	provisionResult := make(chan error, 1)
	go func() {
		_, err := be.ensureBinding(context.Background(), "a", true)
		provisionResult <- err
	}()
	<-provisioning

	// The migration claims b, then holds the slot open through its steer: that is
	// the window this provision's label write runs in, WITHOUT the slot.
	migrated := make(chan struct{})
	releaseMigration := make(chan struct{})
	var migrateOnce sync.Once
	fb.mu.Lock()
	fb.onNotifyMigrated = func() {
		migrateOnce.Do(func() { close(migrated) })
		<-releaseMigration
	}
	fb.mu.Unlock()

	migrationResult := make(chan error, 1)
	go func() {
		migrationResult <- be.migrateHome(context.Background(), "a", "b")
	}()
	<-migrated
	c.Eq("b", be.homeExecutor(), "test setup: the migration claimed the home before the provision wrote")

	// Let the provision finish. Its slot attempt must NOT wait (it publishes the
	// placeholder the holder could be waiting on), so it writes unheld.
	close(releaseProvision)
	c.NoError(<-provisionResult, "losing the home to a migration is not the provision's failure")

	close(releaseMigration)
	c.NoError(<-migrationResult, "the migration keeps its claim, so it reports no error")

	labels := fb.bindingLabels()
	c.Eq("b", be.homeExecutor(), "the migration's executor is the home; home =")
	c.Eq("b", labels["rafiki/executor"],
		"the unheld write lost the home, so the durable pair must name the MIGRATION's executor, not "+
			"the one it labelled: a resume reads these labels; labels %v", labels)
	c.Eq("", labels["rafiki/workspace"],
		"and the losing write's workspace must be taken back rather than left as the home's; labels %v", labels)
	fb.mu.Lock()
	undos := fb.undos
	fb.mu.Unlock()
	c.Eq(1, undos, "exactly one compensation — the write did happen and was then taken back; undos")
	assertNoWorkspaceLeaked(t, be, fb)
}

// The case the rule exists for: the first provision ACQUIRES the home-transition
// slot — it just does so after a migration has already finished and made another
// executor the home. Holding the slot is no defence, because the read this write
// labels on was stale before the slot was taken: "I hold the slot" says only that
// no other transition is running NOW, not that the home still names the executor
// this provision started for.
//
// Only a caller that WILL SET the home (a migration, an adoption) may label ahead
// of its own claim. A provision never does, so its write is judged against the
// settled home and taken back when that home is someone else. Otherwise the
// durable pair names the old executor while rafiki/home-executor names the new
// one — the disagreement a resume reads.
func TestHomeProvisionThatAcquiredTheSlotAfterAMigrationNamesTheMigrationExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder(&fakeExec{id: "a"}, &fakeExec{id: "b"})
	be := newBoundExecutor("c1", fb)
	be.setHome("a")

	// Hold the first provision inside its Provision RPC, before it tries the slot.
	provisioning := make(chan struct{})
	releaseProvision := make(chan struct{})
	var provisionOnce sync.Once
	fb.mu.Lock()
	fb.onProvision = func(id string) {
		if id != "a" {
			return
		}
		provisionOnce.Do(func() { close(provisioning) })
		<-releaseProvision
	}
	fb.mu.Unlock()

	provisionResult := make(chan error, 1)
	go func() {
		_, err := be.ensureBinding(context.Background(), "a", true)
		provisionResult <- err
	}()
	<-provisioning

	// The migration runs to completion -- slot released, home claimed -- while the
	// provision is still inside its Provision RPC.
	c.NoError(be.migrateHome(context.Background(), "a", "b"), "the migration completes")
	c.Eq("b", be.homeExecutor(), "test setup: the home moved before the provision took the slot")

	close(releaseProvision)
	c.NoError(<-provisionResult, "losing the home is not the provision's failure")

	labels := fb.bindingLabels()
	c.Eq("b", be.homeExecutor(), "the migration's executor is still the home; home =")
	c.Eq("b", labels["rafiki/executor"],
		"a provision that took a FREE slot after the home moved is labelling on a stale read, so the "+
			"durable pair must still name the MIGRATION's executor; labels %v", labels)
	c.Eq("", labels["rafiki/workspace"],
		"and its own workspace must be taken back, not left as the home's; labels %v", labels)
	fb.mu.Lock()
	undos := fb.undos
	fb.mu.Unlock()
	c.Eq(1, undos, "exactly one compensation — the write did happen and was then taken back; undos")
	assertNoWorkspaceLeaked(t, be, fb)
}

// --- Lazy relaunch of installed executors -----------------------------------

// installedBinder is a controllerBinder over the install fixture's fakes, bound
// to the fixture's owner. The fixture's pool holds the launcher and nothing
// else; the target executor is reachable through its install row while down.
func (f *installFixture) installedBinder() *controllerBinder {
	return f.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{
		Name: installTestOwnerName, UserID: installTestOwner,
	}).(*controllerBinder)
}

// A NAMED call to a down installed executor relaunches it once, provisions once,
// and tells the model what happened in the one-time note slot.
func TestBoundExecutorRelaunchesOnNamedCall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	// The relayed executor connects as soon as its session opens, which is the
	// event a real relaunch waits for.
	f.connectAs = "exec-1"

	be := newBoundExecutor("c1", f.installedBinder())
	tgt := &tools.ExecutorTarget{Requested: "ublk-s2"}
	ctx := tools.WithExecutorTarget(context.Background(), tgt)

	_, err := be.Execute(ctx, "read", nil)
	c.Require().NoError(err, "a named call to a down installed executor must relaunch it and then work")
	c.Eq(1, f.launcher.serveCalls(), "exactly one ssh session")
	c.Eq(1, len(f.pool.provisionCalls()), "exactly one provision")
	c.Eq(1, strings.Count(tgt.Note, "relaunched"),
		"the relaunch note is written once; note = %q", tgt.Note)
	c.StrContains(tgt.Note,
		"[ublk-s2] relaunched (previous session ended unknown); anything not committed there may be gone",
		"the note names the executor and is honest about not knowing when it ended; note = %q", tgt.Note)
	c.StrContains(tgt.Note, "[ublk-s2] working from",
		"the cwd note is JOINED with the relaunch note, not overwritten; note = %q", tgt.Note)
}

// A relaunch that fails is the call's answer and names the launcher and its ssh
// text — and it leaves the child usable: the failed placeholder is dropped, so
// the next call tries again rather than inheriting a sticky failure.
func TestBoundExecutorRelaunchFailureNamesLauncher(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	// The launcher's durable row exists (so its machine name resolves) but it is
	// not connected: nothing can start the target.
	f.execs.execs[installTestLauncher] = installLauncherLive(
		installTestLauncher, installTestLauncherN, "work", "pinned").Executor
	f.pool.live = nil

	be := newBoundExecutor("c1", f.installedBinder())
	for i := 0; i < 2; i++ {
		_, err := be.Execute(namedCall("ublk-s2"), "read", nil)
		c.Require().Error(err, "call %d: a relaunch with no launcher must fail the call", i)
		c.Eq("[ublk-s2] relaunch failed via greyshift: launcher greyshift is not connected",
			err.Error(), "call %d error text", i)
	}
	c.Eq(0, len(f.pool.provisionCalls()), "nothing may be provisioned on an executor that never came up")
	c.Eq(0, f.launcher.serveCalls(), "and no ssh session was opened")
}

// Eight children of one daemon asking at once cost ONE ssh session: the launch
// is coalesced per executor id, and every caller shares its outcome.
func TestBoundExecutorRelaunchCoalescesConcurrentCalls(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	// Hold the session open and announce nothing: the leader stays inside the
	// launch, which is the only deterministic way to know a follower joined it
	// rather than starting a second one.
	opened := make(chan struct{})
	var once sync.Once
	f.state.serve = func(ctx context.Context, _ net.Conn) error {
		once.Do(func() { close(opened) })
		<-ctx.Done()
		return ctx.Err()
	}

	binder := f.installedBinder()
	leader := make(chan error, 1)
	go func() { leader <- execNamed(newBoundExecutor("c1", binder), "ublk-s2") }()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never opened its ssh session")
	}

	// A second child whose context is already dead: it JOINS the flight and
	// returns its own cancellation, which is only true if it never opened a
	// session of its own.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	follower := newBoundExecutor("c2", binder)
	_, ferr := follower.Execute(tools.WithExecutorTarget(cancelled, &tools.ExecutorTarget{Requested: "ublk-s2"}), "read", nil)
	c.ErrorIs(ferr, context.Canceled, "a coalesced caller returns its own context's error")
	c.Eq(1, f.launcher.serveCalls(), "two callers must cost ONE ssh session")

	f.ctrl.installConnected("exec-1")
	select {
	case err := <-leader:
		c.NoError(err, "the leader completes when the executor connects")
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never returned after the connection was announced")
	}
	c.Eq(1, f.launcher.serveCalls(), "still one session")
}

// execNamed runs one named tool call through be and returns only the error.
func execNamed(be *boundExecutor, executor string) error {
	_, err := be.Execute(namedCall(executor), "read", nil)
	return err
}
