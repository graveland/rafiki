package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"
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

func (f *fakeExec) Execute(_ context.Context, tool string, _ json.RawMessage) (string, error) {
	if f.parent != nil {
		f.parent.mu.Lock()
		f.parent.executeCalls++
		f.parent.mu.Unlock()
	}
	if err := f.next(); err != nil {
		return "", err
	}
	return f.id + ":" + tool, nil
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

// fakeBinder scripts which executor ChooseFor hands back, and records
// provisions and releases so a test can assert exactly one of each.
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

	// mode/live back WorkspaceMode/IsLive for tests that care about the
	// pinned/ephemeral policy rather than about wiring up fakeExecs by hand.
	// live defaults true (see newFakeBinder) so the pre-existing per-id
	// liveByID map keeps deciding liveness unless a test overrides it.
	mode string
	live bool

	// migrations/lastSteer record NotifyMigrated calls.
	migrations int
	lastSteer  string

	// watched/forgetJobs record WatchJob/ForgetJob calls, as "childID|handle|command"
	// triples in call order.
	watched    []string
	forgetJobs []string

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

	// onProvision, when set, runs synchronously inside ProvisionOn -- with
	// f.mu NOT held -- before it does anything else. It lets a test hold a
	// Provision RPC open to prove a caller (recover) isn't holding
	// boundExecutor's lock across it.
	onProvision func()
}

func newFakeBinder(execs ...*fakeExec) *fakeBinder {
	// mode defaults to "ephemeral" so the pre-existing rebind tests, which
	// predate workspace_mode and never set it, keep exercising a migration.
	// Tests that care about the pinned policy set f.mode explicitly.
	fb := &fakeBinder{
		execs:    map[string]*fakeExec{},
		liveByID: map[string]bool{},
		live:     true,
		mode:     "ephemeral",
		released: map[string]bool{},
	}
	for _, e := range execs {
		fb.execs[e.id] = e
		fb.liveByID[e.id] = true
		fb.order = append(fb.order, e.id)
	}
	return fb
}

func (f *fakeBinder) ChooseFor(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chooseCalls++
	if f.chooseErr != nil {
		return "", f.chooseErr
	}
	if len(f.order) == 0 {
		// Nothing was explicitly configured: synthesize a fresh executor each
		// time, so a policy test can tell a re-selection (a NEW id) apart from
		// an in-place re-provision (the SAME id) without wiring up fakeExecs.
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
		return id, nil
	}
	id := f.order[0]
	if len(f.order) > 1 {
		f.order = f.order[1:]
	}
	return id, nil
}

func (f *fakeBinder) ProvisionOn(_ context.Context, id string) (string, tools.ExecutorClient, error) {
	f.mu.Lock()
	hook := f.onProvision
	f.mu.Unlock()
	if hook != nil {
		hook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisions++
	e, ok := f.execs[id]
	if !ok {
		return "", nil, fmt.Errorf("no such executor %q", id)
	}
	// Each call gets a DISTINCT workspace id, matching a real executor: it
	// mints a fresh workspace every time, even for the same executor. A
	// deterministic "ws-"+id here would make an in-place re-provision look
	// like it returned the SAME workspace, hiding the old one never having
	// been released.
	ws := fmt.Sprintf("ws-%s-%d", id, f.provisions)
	f.lastWorkspace = ws
	return ws, e, nil
}

func (f *fakeBinder) ReleaseOn(_ context.Context, _, workspaceID string) {
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

func (f *fakeBinder) NotifyMigrated(_, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrations++
	f.lastSteer = rescheduleSteer
}

func (f *fakeBinder) WatchJob(childID, handle, command string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched = append(f.watched, childID+"|"+handle+"|"+command)
}

func (f *fakeBinder) ForgetJob(childID, handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgetJobs = append(f.forgetJobs, childID+"|"+handle)
}

func TestBoundExecutorBindsLazilyAndSticks(t *testing.T) {
	a := &fakeExec{id: "a"}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	for i := 0; i < 3; i++ {
		if _, err := b.Execute(context.Background(), "read", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if fb.provisions != 1 {
		t.Fatalf("a working executor must be provisioned ONCE and held; got %d provisions", fb.provisions)
	}
}

func TestBoundExecutorDoesNotRebindOnToolFailure(t *testing.T) {
	a := &fakeExec{id: "a", errs: []error{fmt.Errorf("wrapped: %w", execpool.ErrToolFailed)}}
	b2 := &fakeExec{id: "b"}
	fb := newFakeBinder(a, b2)
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "bash", nil)
	if !errors.Is(err, execpool.ErrToolFailed) {
		t.Fatalf("a tool failure must be returned to the caller unchanged, got %v", err)
	}
	if fb.provisions != 1 {
		t.Fatalf("bash exiting nonzero must NEVER migrate a child; got %d provisions", fb.provisions)
	}
}

func TestBoundExecutorRebindsWhenTheExecutorIsGone(t *testing.T) {
	a := &fakeExec{id: "a", errs: []error{fmt.Errorf("wrapped: %w", execpool.ErrExecutorGone)}}
	b2 := &fakeExec{id: "b"}
	fb := newFakeBinder(a, b2)
	b := newBoundExecutor("child-1", fb)

	// Bind to a, then make a "not live" so ErrExecutorGone means the
	// EXECUTOR departed rather than just its workspace.
	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	a.errs = []error{fmt.Errorf("wrapped: %w", execpool.ErrExecutorGone)}
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()

	got, err := b.Execute(context.Background(), "read", nil)
	if err != nil {
		t.Fatalf("a departed executor must be replaced, not surfaced: %v", err)
	}
	if got != "b:read" {
		t.Fatalf("the retry must run on the NEW executor; got %q", got)
	}
}

func TestBoundExecutorReprovisionsOnSameExecutorWhenOnlyWorkspaceIsGone(t *testing.T) {
	a := &fakeExec{id: "a"}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	// a stays LIVE; only its in-memory workspace registry lost the id.
	a.errs = []error{fmt.Errorf("wrapped: %w", execpool.ErrExecutorGone)}

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatalf("a lost workspace on a live executor must be re-provisioned: %v", err)
	}
	if fb.provisions != 2 {
		t.Fatalf("want exactly one re-provision, got %d total provisions", fb.provisions)
	}
	if fb.chooseCalls != 1 {
		t.Fatalf("a lost WORKSPACE must not re-run selection; ChooseFor called %d times", fb.chooseCalls)
	}
}

func TestBoundExecutorRetriesOnlyOnce(t *testing.T) {
	a := &fakeExec{id: "a", errs: []error{
		fmt.Errorf("1: %w", execpool.ErrExecutorGone),
		fmt.Errorf("2: %w", execpool.ErrExecutorGone),
		fmt.Errorf("3: %w", execpool.ErrExecutorGone),
	}}
	fb := newFakeBinder(a)
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "read", nil)
	if err == nil {
		t.Fatal("an executor failing every time must surface the error, not loop")
	}
	if a.calls > 2 {
		t.Fatalf("at most one retry per call; the executor saw %d calls", a.calls)
	}
}

func TestBoundExecutorConcurrentFailuresProduceOneRebind(t *testing.T) {
	a := &fakeExec{id: "a"}
	b2 := &fakeExec{id: "b"}
	fb := newFakeBinder(a, b2)
	b := newBoundExecutor("child-1", fb)

	if _, err := b.Execute(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	fb.liveByID["a"] = false
	fb.mu.Unlock()
	a.mu.Lock()
	for i := 0; i < 8; i++ {
		a.errs = append(a.errs, fmt.Errorf("gone: %w", execpool.ErrExecutorGone))
	}
	a.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Execute(context.Background(), "read", nil)
		}()
	}
	wg.Wait()

	// One provision at bind + exactly one at rebind. Eight concurrent
	// failures must not provision eight workspaces.
	if fb.provisions != 2 {
		t.Fatalf("concurrent failures must coalesce into ONE rebind; got %d provisions", fb.provisions)
	}
}

func TestBoundExecutorUnboundReturnsTheRefusalReason(t *testing.T) {
	fb := newFakeBinder()
	fb.chooseErr = errors.New("spawn refused: no executor satisfies \"machine=xyz\"")
	b := newBoundExecutor("child-1", fb)

	_, err := b.Execute(context.Background(), "read", nil)
	if err == nil {
		t.Fatal("an unbound executor must error")
	}
	if !contains(err.Error(), "no executor satisfies") {
		t.Fatalf("the refusal diagnostic must reach the agent, got %q", err)
	}
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
	if err := b.Ping(context.Background()); err == nil {
		t.Fatal("Ping must refuse when unbound")
	}
}

// Re-provisioning in place leaves the OLD workspace registered on a LIVE
// executor. Unlike the migration branch below it, nothing released it -- so it
// and its retained background-job output were stranded until the executor
// process exited.
func TestReprovisionInPlaceReleasesTheOldWorkspace(t *testing.T) {
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = true
	b := newBoundExecutor("c1", f)
	if _, _, err := b.clientFor(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := f.lastWorkspace

	if !b.recover(context.Background(), b.stale(), execpool.ErrExecutorGone, true) {
		t.Fatal("want a re-provision")
	}
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
	f.live = false
	entered := make(chan struct{})
	release := make(chan struct{})
	f.onProvision = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	b := newBoundExecutor("c1", f)

	go func() { b.recover(context.Background(), "stale/ws", execpool.ErrExecutorLost, true) }()
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

// A successful bash_start is what arms the exit notification; the tool
// description's promise ("you are notified when the job finishes") rests on
// this call landing.
func TestStartJobArmsAWatchThroughTheBinder(t *testing.T) {
	fb := newFakeBinder()
	b := newBoundExecutor("child-1", fb)

	handle, err := b.StartJob(context.Background(), "npm run dev")
	if err != nil {
		t.Fatal(err)
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.watched) != 1 || fb.watched[0] != "child-1|"+handle+"|npm run dev" {
		t.Fatalf("watched = %v, want exactly [child-1|%s|npm run dev]", fb.watched, handle)
	}
}

// A failed start started nothing: a watch on a handle that does not exist
// would poll forever and eventually report a job gone that never ran.
func TestFailedStartJobArmsNothing(t *testing.T) {
	fb := newFakeBinder()
	fb.failWith = errors.New("no such executor")
	b := newBoundExecutor("child-1", fb)

	if _, err := b.StartJob(context.Background(), "npm run dev"); err == nil {
		t.Fatal("expected the scripted failure")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.watched) != 0 {
		t.Fatalf("watched = %v; a failed start must arm no watch", fb.watched)
	}
}

// bash_kill resolved the job on purpose: the watch is dropped so the exit
// never injects a turn's worth of old news.
func TestKillJobForgetsTheWatch(t *testing.T) {
	fb := newFakeBinder()
	b := newBoundExecutor("child-1", fb)

	handle, err := b.StartJob(context.Background(), "npm run dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.KillJob(context.Background(), handle); err != nil {
		t.Fatal(err)
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.forgetJobs) != 1 || fb.forgetJobs[0] != "child-1|"+handle {
		t.Fatalf("forgetJobs = %v, want exactly [child-1|%s]", fb.forgetJobs, handle)
	}
	if len(fb.watched) != 1 {
		t.Fatalf("watched = %v; the kill must not disturb the record of the start", fb.watched)
	}
}

// A kill that never landed (the stream broke) drops nothing: the job may
// still be running, and its exit is still news.
func TestFailedKillJobKeepsTheWatch(t *testing.T) {
	fb := newFakeBinder()
	fb.failWith = errors.New("stream broken")
	b := newBoundExecutor("child-1", fb)

	if err := b.KillJob(context.Background(), "job-1"); err == nil {
		t.Fatal("expected the scripted failure")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.forgetJobs) != 0 {
		t.Fatalf("forgetJobs = %v; a failed kill must not drop the watch", fb.forgetJobs)
	}
}
