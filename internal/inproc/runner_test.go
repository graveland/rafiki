package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/fundi/internal/agent"
	"git.graveland.dev/brent/rafiki/llm"
)

// sampleEndTurn is one scripted assistant message: a completed turn whose text
// the test asserts on. Mirrors internal/agent/engine_test.go's fixture of the
// same name — that one is an unexported const in package agent, so it cannot be
// imported here.
const sampleEndTurn = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
	`"stop_reason":"end_turn","content":[{"type":"text","text":"the fake reply"}],` +
	`"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// writeFakeTurns writes scripted assistant messages as one ndjson file and
// returns its path.
//
// Config.FakeTurns is a PATH to a newline-delimited-JSON file of
// anthropic.Message values (loaded by agent.LoadFakeSender) — NOT literal reply
// text. package agent has its own writeFakeTurns helper, but it is an
// unexported test helper in another package, so this test needs its own.
func writeFakeTurns(t *testing.T, bodies ...string) string {
	t.Helper()
	lines := make([]string, 0, len(bodies))
	for _, b := range bodies {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(b)); err != nil {
			t.Fatalf("compact scripted body: %v", err)
		}
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write scripted turns: %v", err)
	}
	return path
}

// readFramesUntil reads newline-delimited JSON frames from r until it sees one
// whose "type" equals want, or until r hits EOF. Returns every frame read.
func readFramesUntil(t *testing.T, r io.Reader, want string) []string {
	t.Helper()
	var out []string
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out
		}
		out = append(out, string(raw))
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			continue // unparseable, but still recorded above
		}
		if hdr.Type == want {
			return out
		}
	}
}

// TestRunnerDrivesAFakeTurn proves a prompt written to the runner's stdin
// produces a real frame sequence on its stdout, with no subprocess involved.
//
// It asserts on the intermediate frames, not just the terminal one. This repo
// shipped empty message_update frames twice with a fully green suite, both
// times because every assertion targeted message_end.
func TestRunnerDrivesAFakeTurn(t *testing.T) {
	r := New(Options{
		ChildID: "c_inproc",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{
			Model:          "anthropic/claude-sonnet-4-5",
			Cwd:            t.TempDir(),
			Ref:            "c_inproc",
			SpillDir:       t.TempDir(),
			FakeTurns:      writeFakeTurns(t, sampleEndTurn),
			NoSkills:       true,
			NoContextFiles: true,
		},
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// stderr must be a live reader at EOF, never nil: readStderr would panic.
	if stderr == nil {
		t.Fatal("Start returned a nil stderr")
	}
	if _, err := io.ReadAll(stderr); err != nil {
		t.Errorf("stderr read: %v", err)
	}

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"hello"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	done := make(chan []string, 1)
	go func() { done <- readFramesUntil(t, stdout, "agent_end") }()

	var frames []string
	select {
	case frames = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no agent_end frame within 15s")
	}

	joined := strings.Join(frames, "\n")
	for _, want := range []string{"agent_start", "message_end", "agent_end"} {
		if !strings.Contains(joined, want) {
			t.Errorf("frame stream missing %q; got:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "the fake reply") {
		t.Errorf("frame stream missing the fake turn text; got:\n%s", joined)
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	code, sig := r.Wait()
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if sig != "" {
		t.Errorf("signal = %q, want empty for an in-process runner", sig)
	}
	if r.PID() != 0 {
		t.Errorf("PID() = %d, want 0", r.PID())
	}
}

// TestRunnerContainsPanic is the load-bearing test of this phase. A panic in
// one conversation must become that child's exit, not the daemon's. Without the
// recover in run(), this test crashes the test binary rather than failing.
func TestRunnerContainsPanic(t *testing.T) {
	r := New(Options{
		ChildID: "c_panic",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build: func(context.Context, *agent.Frontend, agent.RuntimeOptions) (*agent.Engine, func(), error) {
			panic("boom from the engine builder")
		},
	})

	_, stdout, _, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The panic must close stdout so the daemon's readStdout sees an ordinary
	// EOF and the normal child-exit path runs.
	if _, err := io.ReadAll(stdout); err != nil {
		t.Errorf("stdout should EOF cleanly after a contained panic, got: %v", err)
	}

	code, _ := r.Wait()
	if code == 0 {
		t.Error("exit code = 0 after a panic, want non-zero")
	}
}

// TestRunnerBuildErrorIsAnExit proves a failed build is reported as a non-zero
// exit rather than hanging the daemon's supervise loop forever.
func TestRunnerBuildErrorIsAnExit(t *testing.T) {
	r := New(Options{
		ChildID: "c_builderr",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: "relative/path"}, // BuildRuntime rejects this
	})
	if _, stdout, _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	} else if _, rerr := io.ReadAll(stdout); rerr != nil {
		t.Errorf("stdout read: %v", rerr)
	}
	if code, _ := r.Wait(); code == 0 {
		t.Error("exit code = 0 after a build failure, want non-zero")
	}
}

// hugeEndTurn returns a scripted end_turn assistant message whose text field
// is size bytes of filler, compacted to one JSON line. Used to force enough
// stdout traffic to fill a pipe's kernel buffer without an unbounded number
// of turns.
func hugeEndTurn(t *testing.T, size int) string {
	t.Helper()
	msg := map[string]any{
		"id":          "msg_huge",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-x",
		"stop_reason": "end_turn",
		"content":     []map[string]any{{"type": "text", "text": strings.Repeat("x", size)}},
		"usage":       map[string]any{"input_tokens": 4, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal huge end_turn: %v", err)
	}
	return string(b)
}

// TestRunnerKillUnwedgesFullPipe is the single most valuable test in this
// package. It reproduces the production-reachable wedge Finding 1 describes:
// Child.readStdout stops reading on any non-EOF error (e.g. a frame too
// large) and immediately calls Wait(), so nobody drains the runner's stdout
// pipe from that point on. Once the kernel buffer fills, the engine's next
// Frontend.Emit blocks in the write syscall WHILE HOLDING Emit's mutex (see
// runner.go's Start doc comment), which means run() never returns,
// close(r.done) never fires, and Wait() blocks forever — and Terminate alone
// cannot break that, because a goroutine blocked in write(2) does not
// observe context cancellation.
//
// The only fix is closing the read end of stdout out from under the blocked
// writer, which is exactly what Kill does. This test proves Kill remains
// effective even when the pipe is fully wedged: it stops reading stdout,
// drives enough huge frames to fill the pipe and block the engine, confirms
// Wait() does NOT return on its own, then calls Kill() and requires Wait() to
// return within a few seconds.
func TestRunnerKillUnwedgesFullPipe(t *testing.T) {
	// 4 MiB comfortably exceeds any realistic OS pipe buffer (tens of KB on
	// Linux and macOS), so a handful of these frames is certain to fill it.
	const hugeSize = 4 << 20
	turns := make([]string, 8)
	for i := range turns {
		turns[i] = hugeEndTurn(t, hugeSize)
	}

	r := New(Options{
		ChildID: "c_wedge",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{
			Model:          "anthropic/claude-sonnet-4-5",
			Cwd:            t.TempDir(),
			SpillDir:       t.TempDir(),
			FakeTurns:      writeFakeTurns(t, turns...),
			NoSkills:       true,
			NoContextFiles: true,
		},
	})

	// stdout is intentionally discarded: the whole scenario is a daemon that
	// stops reading a child's stdout, and the Runner (via r.stdoutR) retains
	// its own reference to the same underlying file, so not keeping a local
	// one here doesn't risk an early finalizer-driven close.
	stdin, _, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}

	// Deliberately never read stdout after this point — that's the
	// scenario. Drive one prompt per scripted huge turn; the engine only
	// needs to get partway through before its stdout write blocks.
	for i := range turns {
		if _, err := stdin.Write([]byte(fmt.Sprintf(`{"type":"prompt","message":"go %d"}`, i) + "\n")); err != nil {
			// A blocked engine can also make the daemon's own stdin write
			// block if the pipe's write-side buffer is also full; either way,
			// once we've stopped reading stdout the wedge is already
			// underway, so treat a blocked/failed further write as fine and
			// move on to proving Wait() hangs.
			break
		}
	}

	waitDone := make(chan struct{})
	go func() {
		r.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("Wait() returned on its own; the pipe never wedged (test needs a larger payload)")
	case <-time.After(2 * time.Second):
		// Expected: the engine is blocked writing to a full, undrained pipe.
	}

	if err := r.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5s of Kill(); Kill failed to unwedge the blocked write")
	}

	if err := stdin.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Errorf("close stdin: %v", err)
	}
}

// blockingToolSet is a minimal agentloop.ToolSet (structurally, not by
// import — see below) with one tool, "bash", that signals started and then
// blocks until its context is cancelled. It mirrors
// internal/agent/engine_test.go's blockOnCtxTool/fakeToolSet, duplicated here
// because those are unexported test helpers in another package.
type blockingToolSet struct {
	started chan struct{}
	once    sync.Once
}

func (*blockingToolSet) Definitions() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "bash",
		InputSchema: anthropic.ToolInputSchemaParam{Type: "object"},
	}}}
}

func (ts *blockingToolSet) Execute(ctx context.Context, name string, _ json.RawMessage) (string, error) {
	if name != "bash" {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	ts.once.Do(func() { close(ts.started) })
	<-ctx.Done()
	return "", ctx.Err()
}

// ctxCheckingSender fails fast on an already-cancelled context before
// delegating, mirroring internal/agent/engine_test.go's sender of the same
// name — the plain fake sender loaded by agent.LoadFakeSender ignores ctx
// entirely, which would let a turn's Continue call silently succeed via the
// replay script even after an abort landed.
type ctxCheckingSender struct{ inner llm.Sender }

func (s ctxCheckingSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.inner.New(ctx, params)
}

// blockingBuildFunc returns a BuildFunc that constructs a real agent.Engine
// (bypassing agent.BuildRuntime) wired to a single "bash" tool that blocks on
// its context until cancelled. It is the inproc package's equivalent of
// internal/agent/engine_test.go's newTestEngineWithSender, injected through
// Options.Build — the same seam TestRunnerContainsPanic already uses to
// substitute a builder a real one can't produce on demand — because
// RuntimeOptions/BuildRuntime give no way to inject a custom tool registry.
func blockingBuildFunc(started chan struct{}, fakeTurnsPath string) BuildFunc {
	return func(ctx context.Context, fe *agent.Frontend, _ agent.RuntimeOptions) (*agent.Engine, func(), error) {
		sender, err := agent.LoadFakeSender(fakeTurnsPath)
		if err != nil {
			return nil, nil, err
		}
		client, err := llm.NewClient(
			llm.WithUpstream(llm.UpstreamAnthropic, ctxCheckingSender{inner: sender}),
			llm.WithDefaultModel("claude-x"))
		if err != nil {
			return nil, nil, err
		}
		eng, err := agent.NewEngine(agent.EngineConfig{
			Client:   client,
			Tools:    &blockingToolSet{started: started},
			Provider: "anthropic",
			ModelID:  "claude-x",
			Name:     "w1",
			BaseCtx:  ctx,
			ConvOpts: []llm.ConvOption{llm.NewConversation("fundi", "agent")},
		}, fe)
		if err != nil {
			return nil, nil, err
		}
		return eng, func() {}, nil
	}
}

// sampleToolUseResp is a scripted assistant message that calls the "bash"
// tool blockingToolSet provides, putting a turn genuinely in flight (the tool
// blocks on ctx) rather than replaying instantaneously the way a plain
// end_turn body would. Mirrors internal/agent/emit_test.go's sampleResp,
// duplicated here because that one is an unexported const in package agent.
const sampleToolUseResp = `{"id":"msg_0","type":"message","role":"assistant","model":"claude-x",` +
	`"stop_reason":"tool_use","content":[{"type":"text","text":"on it"},` +
	`{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],` +
	`"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// decodeUntilWithTimeout decodes ndjson frames from dec — a single decoder
// reused across multiple prompts within one test — until it sees a frame
// whose "type" equals want, or fails the test after timeout. A fresh
// json.Decoder per call, as the package-level readFramesUntil above
// constructs, would silently discard whatever the previous decoder had
// already buffered past the last complete frame it returned: fine for a
// test that reads exactly once, but wrong for a test that keeps reading
// after a second prompt. Reusing one decoder for the whole test avoids that.
func decodeUntilWithTimeout(t *testing.T, dec *json.Decoder, want string, timeout time.Duration) []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() {
		var out []string
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				done <- out
				return
			}
			out = append(out, string(raw))
			var hdr struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &hdr); err != nil {
				continue
			}
			if hdr.Type == want {
				done <- out
				return
			}
		}
	}()
	select {
	case frames := <-done:
		return frames
	case <-time.After(timeout):
		t.Fatalf("no %q frame within %s", want, timeout)
		return nil
	}
}

// TestRunnerInterruptAbortsTurnButRunnerStaysAlive covers requirement 5's
// Interrupt half end to end through the Runner: Interrupt must abort an
// in-flight turn cleanly (no agent_error — an abort is not a loop failure)
// while leaving the runner alive and able to take another prompt. Confusing
// Interrupt with Terminate/Kill would make every user abort kill the child,
// which is exactly what this test guards against.
func TestRunnerInterruptAbortsTurnButRunnerStaysAlive(t *testing.T) {
	started := make(chan struct{})
	r := New(Options{
		ChildID: "c_interrupt",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build:   blockingBuildFunc(started, writeFakeTurns(t, sampleToolUseResp, sampleEndTurn)),
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}
	dec := json.NewDecoder(stdout)

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"go"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started; the turn never went in flight")
	}

	if err := r.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	first := decodeUntilWithTimeout(t, dec, "agent_end", 15*time.Second)
	joined := strings.Join(first, "\n")
	if !strings.Contains(joined, "tool_execution_end") {
		t.Errorf("frames after Interrupt missing tool_execution_end; got:\n%s", joined)
	}
	if strings.Contains(joined, "agent_error") {
		t.Errorf("Interrupt must not produce agent_error (it is not a loop failure); got:\n%s", joined)
	}

	// The runner must stay alive and accept another prompt -- the entire
	// point of Interrupt over Terminate/Kill.
	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"second"}` + "\n")); err != nil {
		t.Fatalf("write second prompt: %v", err)
	}
	second := decodeUntilWithTimeout(t, dec, "agent_end", 15*time.Second)
	if !strings.Contains(strings.Join(second, "\n"), "the fake reply") {
		t.Errorf("second turn missing the fake reply from sampleEndTurn; got:\n%s", strings.Join(second, "\n"))
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	if code, sig := r.Wait(); code != 0 || sig != "" {
		t.Errorf("Wait() = (%d, %q), want (0, \"\") after a clean stdin EOF", code, sig)
	}
}

// TestRunnerTerminateEndsIt covers requirement 5's Terminate half: Terminate
// cancels the engine's context, ending the in-flight turn, but — unlike
// Kill — leaves stdin open, so the runner itself only ends once the daemon
// also closes stdin. That two-step (Terminate then close stdin) is exactly
// what child.Runner's doc comment describes as the escalation the daemon
// performs before falling back to Kill.
func TestRunnerTerminateEndsIt(t *testing.T) {
	started := make(chan struct{})
	r := New(Options{
		ChildID: "c_terminate",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build:   blockingBuildFunc(started, writeFakeTurns(t, sampleToolUseResp)),
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"go"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started; the turn never went in flight")
	}

	if err := r.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// Terminate cancels ctx but does not touch the pipes, so Frontend.Run
	// keeps blocking on its next stdin read: the runner as a whole must NOT
	// end until stdin is also closed.
	waitDone := make(chan struct{})
	go func() {
		r.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait() returned before stdin was closed; Terminate alone must not end the runner")
	case <-time.After(200 * time.Millisecond):
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5s of closing stdin after Terminate")
	}

	// Terminate only cancels the in-flight turn's context; it does not itself
	// record a failing exit (run() only sets a non-zero code on a build error
	// or a Frontend.Run error, neither of which happened here) — a clean
	// stdin EOF after Terminate is still an ordinary exit 0, same as any
	// other clean shutdown.
	if code, sig := r.Wait(); code != 0 || sig != "" {
		t.Errorf("Wait() = (%d, %q), want (0, \"\") after a clean stdin EOF", code, sig)
	}
	if _, err := io.ReadAll(stdout); err != nil {
		t.Errorf("stdout read: %v", err)
	}
}

// cancelCtxChildCount reports how many child contexts ctx still has
// registered, by reflecting on context.cancelCtx's unexported `children` map.
// ok is false when that internal shape is not what we expect (a future Go
// release could rename or restructure it), so the caller can fall back to the
// public-API assertion rather than failing for the wrong reason.
//
// reflect.Value.Len does not require the field to be exported — only
// Interface() does — so reading the map's length here is legal.
func cancelCtxChildCount(ctx context.Context) (int, bool) {
	v := reflect.ValueOf(ctx)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, false
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("children")
	if !f.IsValid() || f.Kind() != reflect.Map {
		return 0, false
	}
	return f.Len(), true
}

// TestRunnerReleasesChildContextOnNormalCompletion guards the leak that
// nothing else in this package can see: run() derives a cancellable context
// from Options.Parent, and until this test existed cancel() was invoked only
// by Terminate, Kill, and the panic arm of the recover defer. An ordinary
// completed child therefore left its cancelCtx registered in the parent's
// children map forever, and the daemon passes a REAL cancelCtx
// (Controller.baseCtx) as Parent — one leaked context per completed
// conversation, for the daemon's lifetime.
//
// Every other test in this file passes t.Context() or lets New default Parent
// to context.Background(); Background's Done() is nil, so propagateCancel
// registers nothing and the leak is structurally invisible there. This test
// therefore builds its own context.WithCancel(context.Background()) parent —
// that IS the shape the daemon uses.
//
// It asserts two ways: the public-API one (a context.AfterFunc registered on
// the child ctx inside Build must fire, which can only happen if the child ctx
// was cancelled), and the direct one (the parent's child count returns to
// zero), the latter only when the context package's internals are still the
// shape cancelCtxChildCount expects.
func TestRunnerReleasesChildContextOnNormalCompletion(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	const children = 4
	fired := make(chan string, children)
	turns := writeFakeTurns(t, sampleEndTurn)

	for i := range children {
		id := fmt.Sprintf("c_ctxleak_%d", i)
		inner := blockingBuildFunc(make(chan struct{}), turns)
		r := New(Options{
			ChildID: id,
			Parent:  parent,
			Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
			Build: func(ctx context.Context, fe *agent.Frontend, ro agent.RuntimeOptions) (*agent.Engine, func(), error) {
				context.AfterFunc(ctx, func() { fired <- id })
				return inner(ctx, fe, ro)
			},
		})

		stdin, stdout, stderr, err := r.Start()
		if err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
		if err := stderr.Close(); err != nil {
			t.Errorf("close stderr %s: %v", id, err)
		}
		// Drain stdout so the engine can never block on a full pipe; this test
		// is about the ordinary completion path, nothing else.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			if _, cerr := io.Copy(io.Discard, stdout); cerr != nil {
				t.Errorf("drain stdout %s: %v", id, cerr)
			}
		}()

		// No prompt at all: closing stdin immediately is the shortest possible
		// ordinary completion — Frontend.Run sees EOF, run() returns normally.
		if err := stdin.Close(); err != nil {
			t.Fatalf("close stdin %s: %v", id, err)
		}
		if code, sig := r.Wait(); code != 0 || sig != "" {
			t.Fatalf("Wait() = (%d, %q) for %s, want (0, \"\") on a clean EOF", code, sig, id)
		}
		<-drained
	}

	for range children {
		select {
		case <-fired:
		case <-time.After(10 * time.Second):
			t.Fatal("a child context was never cancelled after its runner completed normally; " +
				"run()'s cancel() is not running on the ordinary return path")
		}
	}

	if got, ok := cancelCtxChildCount(parent); !ok {
		t.Log("context.cancelCtx internals changed shape; relying on the AfterFunc assertion above")
	} else if got != 0 {
		t.Errorf("parent context still has %d registered children after %d runners completed normally; want 0",
			got, children)
	}
}

// TestRunnerStartIsSingleShot guards a daemon-fatal panic. A second Start()
// would launch a second run() goroutine over the same Runner, and that
// goroutine's `defer close(r.done)` closes an already-closed channel. Because
// that defer is registered BEFORE run's recover defer, the resulting panic is
// NOT contained — it kills the whole daemon rather than one child.
func TestRunnerStartIsSingleShot(t *testing.T) {
	r := New(Options{
		ChildID: "c_twice",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build:   blockingBuildFunc(make(chan struct{}), writeFakeTurns(t, sampleEndTurn)),
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}

	if _, _, _, err := r.Start(); err == nil {
		t.Fatal("second Start returned nil error; it must be rejected, not run() twice")
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	if _, err := io.ReadAll(stdout); err != nil {
		t.Errorf("stdout read: %v", err)
	}
	if code, sig := r.Wait(); code != 0 || sig != "" {
		t.Errorf("Wait() = (%d, %q), want (0, \"\") - the rejected second Start must not have disturbed the first", code, sig)
	}
}
