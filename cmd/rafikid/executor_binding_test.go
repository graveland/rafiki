package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"
)

// fakeExec is one executor's client. Each call returns the next queued error
// (nil means success), so a test can script "fail once, then succeed".
type fakeExec struct {
	id      string
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
	if err := f.next(); err != nil {
		return "", err
	}
	return f.id + ":" + tool, nil
}

func (f *fakeExec) StartJob(_ context.Context, command string) (string, error) {
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

func (f *fakeExec) KillJob(_ context.Context, _ string) error  { return f.next() }
func (f *fakeExec) Ping(_ context.Context) error               { return f.next() }
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
	order       []string            // ChooseFor returns these in sequence
	chooseErr   error               // when set, ChooseFor always fails
	execs       map[string]*fakeExec
	live        map[string]bool
	provisions  int
	releases    int
	noted       []string
	chooseCalls int
}

func newFakeBinder(execs ...*fakeExec) *fakeBinder {
	fb := &fakeBinder{execs: map[string]*fakeExec{}, live: map[string]bool{}}
	for _, e := range execs {
		fb.execs[e.id] = e
		fb.live[e.id] = true
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
		return "", errors.New("no executor")
	}
	id := f.order[0]
	if len(f.order) > 1 {
		f.order = f.order[1:]
	}
	return id, nil
}

func (f *fakeBinder) ProvisionOn(_ context.Context, id string) (string, tools.ExecutorClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisions++
	e, ok := f.execs[id]
	if !ok {
		return "", nil, fmt.Errorf("no such executor %q", id)
	}
	return "ws-" + id, e, nil
}

func (f *fakeBinder) ReleaseOn(_ context.Context, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
}

func (f *fakeBinder) IsLive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

func (f *fakeBinder) NoteBinding(_, execID, wsID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noted = append(f.noted, execID+"/"+wsID)
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
	fb.live["a"] = false
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
	fb.live["a"] = false
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

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}