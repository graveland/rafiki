package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakeStartRecord is one Backgrounder.Start call, captured so a test can
// assert what the bash tool handed off.
type fakeStartRecord struct {
	argv    []string
	env     []string
	dir     string
	command string
	owner   string
}

// fakeBackgrounder is a Backgrounder whose per-call behaviour the test
// supplies, so a test drives the provisional (finish, promote, cancel) rather
// than depending on a real process.
type fakeBackgrounder struct {
	mu    sync.Mutex
	calls []fakeStartRecord
	fn    func(fakeStartRecord) (Provisional, error)
}

func (f *fakeBackgrounder) Start(argv, env []string, dir, command, owner string) (Provisional, error) {
	rec := fakeStartRecord{argv: argv, env: env, dir: dir, command: command, owner: owner}
	f.mu.Lock()
	f.calls = append(f.calls, rec)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("fakeBackgrounder: no fn configured")
	}
	return fn(rec)
}

func (f *fakeBackgrounder) records() []fakeStartRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeStartRecord(nil), f.calls...)
}

// fakeProvisional is a Provisional the test controls. Its Done channel is
// closed by finish; until then it is a command that "has not exited".
type fakeProvisional struct {
	done chan struct{}

	mu        sync.Mutex
	combined  string
	stderr    string
	runErr    error
	snap      string
	last      time.Time
	handle    string
	promoted  bool
	discarded bool
}

func newFakeProvisional(handle string) *fakeProvisional {
	return &fakeProvisional{done: make(chan struct{}), handle: handle}
}

func (p *fakeProvisional) finish(combined, stderr string, err error) {
	p.mu.Lock()
	p.combined, p.stderr, p.runErr = combined, stderr, err
	p.mu.Unlock()
	close(p.done)
}

func (p *fakeProvisional) Done() <-chan struct{} { return p.done }

func (p *fakeProvisional) Result() (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.combined, p.stderr, p.runErr
}

func (p *fakeProvisional) Snapshot() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap, p.last
}

func (p *fakeProvisional) Promote() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.promoted = true
	return p.handle
}

func (p *fakeProvisional) Discard() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discarded = true
}

func (p *fakeProvisional) wasPromoted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.promoted
}

func (p *fakeProvisional) wasDiscarded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discarded
}

// testAutoBashTool materializes a bash tool wired to bg.
func testAutoBashTool(t *testing.T, bg Backgrounder) Tool {
	t.Helper()
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
		Backgrounder: bg,
	})
	assert.NewAborting(t).NoError(err)
	return tool
}

// realExitError yields a genuine *exec.ExitError, the shape exec.Cmd.Wait
// reports for a nonzero exit.
func realExitError(t *testing.T) error {
	t.Helper()
	var ee *exec.ExitError
	if err := exec.Command("bash", "-c", "exit 1").Run(); !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %v", err)
	}
	return ee
}

// TestAutoBackgroundFinishedUnderThresholdReturnsNormalResult: a command that
// finishes before its timeout is returned exactly as the old path would, and
// its provisional is discarded, never promoted.
func TestAutoBackgroundFinishedUnderThresholdReturnsNormalResult(t *testing.T) {
	c := assert.NewAborting(t)
	p := newFakeProvisional("h1")
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) {
		p.finish("hello\n", "", nil)
		return p, nil
	}}
	tool := testAutoBashTool(t, fbg)

	ctx := WithAutoBackground(context.Background(), "owner-1")
	res, err := tool.Execute(ctx, ToolInput(json.RawMessage(`{"command":"echo hello","timeout_ms":5000}`)))
	c.NoError(err)
	c.StrContains(res.Text, "hello", "expected the command's output, got")
	c.False(!p.wasDiscarded(), "Discard must be called on a finished provisional")
	c.False(p.wasPromoted(), "Promote must not be called when the command finished in time")
}

// TestAutoBackgroundPromotesAtThreshold: a command still running at its
// timeout is promoted to a job, and the model receives the handoff text naming
// the handle and how to read more.
func TestAutoBackgroundPromotesAtThreshold(t *testing.T) {
	c := assert.NewAborting(t)
	p := newFakeProvisional("h1") // never finishes
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) { return p, nil }}
	tool := testAutoBashTool(t, fbg)

	ctx := WithAutoBackground(context.Background(), "owner-1")
	res, err := tool.Execute(ctx, ToolInput(json.RawMessage(`{"command":"sleep 5","timeout_ms":50}`)))
	c.NoError(err)
	c.StrContains(res.Text, "moved to the background as job h1", "expected the handoff naming the job, got")
	c.StrContains(res.Text, `bash_output {"handle":"h1"}`, "expected a bash_output hint, got")
	c.StrContains(res.Text, "still running after 50ms", "expected the raw, unrounded timeout in the handoff, got")
	c.False(!p.wasPromoted(), "Promote must be called at the threshold")
	c.False(p.wasDiscarded(), "a promoted job must not be discarded")
}

// TestAutoBackgroundCancelledCallDiscards: a caller that cancels mid-flight
// gets the provisional discarded and never promoted.
func TestAutoBackgroundCancelledCallDiscards(t *testing.T) {
	c := assert.NewAborting(t)
	p := newFakeProvisional("h1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) {
		cancel()
		return p, nil
	}}
	tool := testAutoBashTool(t, fbg)

	_, _ = tool.Execute(WithAutoBackground(ctx, "owner-1"), ToolInput(json.RawMessage(`{"command":"sleep 5"}`)))
	c.False(!p.wasDiscarded(), "a cancelled call must Discard the provisional")
	c.False(p.wasPromoted(), "a cancelled call must not Promote")
}

// TestAutoBackgroundOffWithoutCtxFlag: without WithAutoBackground the tool
// takes the ordinary bash path and never touches the Backgrounder.
func TestAutoBackgroundOffWithoutCtxFlag(t *testing.T) {
	c := assert.NewAborting(t)
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) {
		t.Fatal("Start must not be called without WithAutoBackground")
		return nil, nil
	}}
	tool := testAutoBashTool(t, fbg)

	res, err := tool.Execute(context.Background(), ToolInput(json.RawMessage(`{"command":"echo hi"}`)))
	c.NoError(err)
	c.StrContains(res.Text, "hi", "expected the ordinary bash path, got")
	c.Len(fbg.records(), 0, "Start must not be called without the ctx flag")
}

// TestAutoBackgroundOffWithoutBackgrounder: the flag is set but no
// Backgrounder was wired, so bash keeps kill-at-timeout.
func TestAutoBackgroundOffWithoutBackgrounder(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
	})
	c.NoError(err)

	ctx := WithAutoBackground(context.Background(), "owner-1")
	res, err := tool.Execute(ctx, ToolInput(json.RawMessage(`{"command":"echo hi"}`)))
	c.NoError(err)
	c.StrContains(res.Text, "hi", "expected the ordinary bash path with no Backgrounder, got")
}

// TestAutoBackgroundRtkRefusalFallsBack: an rtk refusal on the auto path
// re-runs the ORIGINAL command through the same auto path, now as
// `bash -c <original>`.
func TestAutoBackgroundRtkRefusalFallsBack(t *testing.T) {
	c := assert.NewAborting(t)
	cleanup := fakeRTK(t)
	defer cleanup()
	exitErr := realExitError(t)

	first := newFakeProvisional("h1")
	second := newFakeProvisional("h2")
	var n int
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) {
		n++
		if n == 1 {
			first.finish("", "rtk: rtk find does not support compound predicates", exitErr)
			return first, nil
		}
		second.finish("needle.go\n", "", nil)
		return second, nil
	}}
	tool := testAutoBashTool(t, fbg)

	ctx := WithAutoBackground(context.Background(), "owner-1")
	res, err := tool.Execute(ctx, ToolInput(json.RawMessage(`{"command":"find . -name '*.go'"}`)))
	c.NoError(err)
	c.StrContains(res.Text, "needle.go", "expected the plain-bash fallback output, got")

	recs := fbg.records()
	c.Len(recs, 2, "expected a rewrite attempt followed by a bash fallback")
	c.Eq("rtk", recs[0].argv[0], "the first Start should run the rtk rewrite, got")
	c.Eq("-c", recs[1].argv[1], "the fallback argv should be bash -c <original>, got")
	c.Eq("find . -name '*.go'", recs[1].argv[2], "the fallback should re-run the original command, got")
	c.StrContains(recs[1].argv[0], "bash", "the fallback should run under bash, got")
}

// TestAutoBackgroundHandoffSaysNoOutputYet: a promoted command that has
// written nothing yet says so rather than inventing an age.
func TestAutoBackgroundHandoffSaysNoOutputYet(t *testing.T) {
	c := assert.NewAborting(t)
	p := newFakeProvisional("h9") // no snapshot, never finishes
	fbg := &fakeBackgrounder{fn: func(fakeStartRecord) (Provisional, error) { return p, nil }}
	tool := testAutoBashTool(t, fbg)

	ctx := WithAutoBackground(context.Background(), "owner-1")
	res, err := tool.Execute(ctx, ToolInput(json.RawMessage(`{"command":"sleep 5","timeout_ms":50}`)))
	c.NoError(err)
	c.StrContains(res.Text, "[no output yet]", "expected the no-output-yet marker, got")
}

// TestBashDescriptionMentionsBackgroundAndKill pins the contract change in the
// tool description the model sees.
func TestBashDescriptionMentionsBackgroundAndKill(t *testing.T) {
	c := assert.NewAborting(t)
	desc := BashBlueprint{}.Description()
	c.StrContains(desc, "moved to the background", "description must mention auto-background, got")
	c.StrContains(desc, "bash_kill", "description must mention bash_kill, got")
	c.False(strings.Contains(desc, "Defaults to a 120s timeout"), "the old timeout sentence must be gone, got")
}
