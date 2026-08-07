package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/llm"
)

// sampleEndTurn is one scripted assistant message: a completed turn whose text
// the test asserts on. Mirrors internal/fundi/engine_test.go's fixture of the
// same name — that one is an unexported const in package fundi, so it cannot be
// imported here.
const sampleEndTurn = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
	`"stop_reason":"end_turn","content":[{"type":"text","text":"the fake reply"}],` +
	`"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// writeFakeTurns writes scripted assistant messages as one ndjson file and
// returns its path.
//
// Config.FakeTurns is a PATH to a newline-delimited-JSON file of
// anthropic.Message values (loaded by fundi.LoadFakeSender) — NOT literal reply
// text. package fundi has its own writeFakeTurns helper, but it is an
// unexported test helper in another package, so this test needs its own.
// panickingBashTool is a tools.Tool whose Execute always panics.
type panickingBashTool struct{}

func (panickingBashTool) Name() string              { return "bash" }
func (panickingBashTool) Description() string       { return "panics on purpose" }
func (panickingBashTool) InputSchema() tools.Schema { return tools.Schema{Type: "object"} }
func (panickingBashTool) Execute(context.Context, tools.ToolInput) (tools.ToolResult, error) {
	panic("tool exploded inside a real turn")
}

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
		Runtime: fundi.RuntimeOptions{
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
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
		Build: func(context.Context, *fundi.Frontend, fundi.RuntimeOptions) (*fundi.Engine, func(), error) {
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
		Runtime: fundi.RuntimeOptions{Cwd: "relative/path"}, // BuildRuntime rejects this
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
	// 256 KiB is 4x the largest OS pipe buffer (64 KiB on Linux and macOS), so
	// the first frame alone already blocks a writer nobody is draining.
	//
	// This was 4 MiB, which is 64x the buffer and sounds harmlessly generous —
	// but 8 turns of it is a 32 MiB scripted-turns file, and fundi.LoadFakeSender
	// parses the whole thing eagerly and synchronously inside Options.Build.
	// That takes ~0.7s normally and ~6.5s under -race, which is longer than this
	// test's entire Kill deadline. So under -race run() was still in
	// encoding/json when Kill() landed, the engine had not emitted a single
	// byte, the pipe was never filled, and nothing was ever wedged: the test
	// failed against a CPU-bound parse rather than the mechanism it names, and
	// on the runs where it passed it had proved nothing at all. Keep this
	// comfortably above the pipe buffer and well below anything that makes
	// parsing the fixture a factor.
	const hugeSize = 256 << 10
	turns := make([]string, 8)
	for i := range turns {
		turns[i] = hugeEndTurn(t, hugeSize)
	}

	r := New(Options{
		ChildID: "c_wedge",
		Parent:  t.Context(),
		Runtime: fundi.RuntimeOptions{
			Model:          "anthropic/claude-sonnet-4-5",
			Cwd:            t.TempDir(),
			SpillDir:       t.TempDir(),
			FakeTurns:      writeFakeTurns(t, turns...),
			NoSkills:       true,
			NoContextFiles: true,
		},
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}

	// Drive one prompt per scripted huge turn; the engine only needs to get
	// partway through before its stdout write blocks.
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

	// Require exactly one byte from the engine before going silent. Without
	// this the test can pass having proved nothing: if the engine never emits
	// at all then the pipe is never filled, nothing is ever wedged, and the
	// "Wait() did not return within 2s" check below is satisfied by any
	// slowness whatsoever — which is exactly how a 6.5s fixture parse
	// masqueraded as a wedge for as long as the payload was oversized. A byte
	// on stdout proves Build returned, fe.Run() is running and the engine is
	// writing, so the block that follows really is a write wedge and not a
	// stalled startup. One byte and no more: draining any further would
	// relieve the very back-pressure the test depends on.
	firstByte := make(chan error, 1)
	go func() {
		_, err := stdout.Read(make([]byte, 1))
		firstByte <- err
	}()
	select {
	case err := <-firstByte:
		if err != nil {
			t.Fatalf("reading the engine's first stdout byte: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("engine emitted nothing within 30s; the write wedge under test was never reached")
	}

	// Deliberately never read stdout past this point — that is the scenario:
	// a daemon that has stopped draining its child's stdout.

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
// internal/fundi/engine_test.go's blockOnCtxTool/fakeToolSet, duplicated here
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
// delegating, mirroring internal/fundi/engine_test.go's sender of the same
// name — the plain fake sender loaded by fundi.LoadFakeSender ignores ctx
// entirely, which would let a turn's Continue call silently succeed via the
// replay script even after an abort landed.
type ctxCheckingSender struct{ inner llm.Sender }

func (s ctxCheckingSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.inner.New(ctx, params)
}

// blockingBuildFunc returns a BuildFunc that constructs a real fundi.Engine
// (bypassing fundi.BuildRuntime) wired to a single "bash" tool that blocks on
// its context until cancelled. It is the inproc package's equivalent of
// internal/fundi/engine_test.go's newTestEngineWithSender, injected through
// Options.Build — the same seam TestRunnerContainsPanic already uses to
// substitute a builder a real one can't produce on demand — because
// RuntimeOptions/BuildRuntime give no way to inject a custom tool registry.
func blockingBuildFunc(started chan struct{}, fakeTurnsPath string) BuildFunc {
	return func(ctx context.Context, fe *fundi.Frontend, _ fundi.RuntimeOptions) (*fundi.Engine, func(), error) {
		sender, err := fundi.LoadFakeSender(fakeTurnsPath)
		if err != nil {
			return nil, nil, err
		}
		client, err := llm.NewClient(
			llm.WithUpstream(llm.UpstreamAnthropic, ctxCheckingSender{inner: sender}),
			llm.WithDefaultModel("claude-x"))
		if err != nil {
			return nil, nil, err
		}
		eng, err := fundi.NewEngine(fundi.EngineConfig{
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
// end_turn body would. Mirrors internal/fundi/emit_test.go's sampleResp,
// duplicated here because that one is an unexported const in package fundi.
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
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
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
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
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
			Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
			Build: func(ctx context.Context, fe *fundi.Frontend, ro fundi.RuntimeOptions) (*fundi.Engine, func(), error) {
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
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
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

// panicToolBuildFunc returns a BuildFunc whose engine is wired to a REAL
// tools.Registry carrying one "bash" tool that panics. Using the real Registry
// is the point: the containment under test lives in Registry.Execute, and a
// hand-rolled ToolSet in this test would bypass it and prove nothing.
//
// It forwards ro.OnFatal to EngineConfig, exactly as fundi.BuildRuntime does,
// so the child-ending path stays wired for an injected build.
func panicToolBuildFunc(fakeTurnsPath string) BuildFunc {
	return func(ctx context.Context, fe *fundi.Frontend, ro fundi.RuntimeOptions) (*fundi.Engine, func(), error) {
		sender, err := fundi.LoadFakeSender(fakeTurnsPath)
		if err != nil {
			return nil, nil, err
		}
		client, err := llm.NewClient(
			llm.WithUpstream(llm.UpstreamAnthropic, sender),
			llm.WithDefaultModel("claude-x"))
		if err != nil {
			return nil, nil, err
		}
		reg := tools.NewRegistry()
		reg.Register(panickingBashTool{})
		eng, err := fundi.NewEngine(fundi.EngineConfig{
			Client:   client,
			Tools:    reg,
			Provider: "anthropic",
			ModelID:  "claude-x",
			Name:     "w1",
			BaseCtx:  ctx,
			OnFatal:  ro.OnFatal,
			ConvOpts: []llm.ConvOption{llm.NewConversation("fundi", "agent")},
		}, fe)
		if err != nil {
			return nil, nil, err
		}
		return eng, func() {}, nil
	}
}

// TestRunnerSurvivesAPanickingTool proves the tool-execution boundary end to
// end: a tool body that panics becomes a failed tool result on the wire, the
// turn still completes, and the child is still alive and able to take another
// prompt. Without the recover in tools.Registry.Execute this test does not
// fail — it crashes the test binary, because agentloop runs the tool on an
// errgroup goroutine that nothing recovers.
func TestRunnerSurvivesAPanickingTool(t *testing.T) {
	r := New(Options{
		ChildID: "c_toolpanic",
		Parent:  t.Context(),
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
		Build:   panicToolBuildFunc(writeFakeTurns(t, sampleToolUseResp, sampleEndTurn, sampleEndTurn)),
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

	first := strings.Join(decodeUntilWithTimeout(t, dec, "agent_end", 15*time.Second), "\n")
	if !strings.Contains(first, "tool_execution_end") {
		t.Errorf("no tool_execution_end after a panicking tool; got:\n%s", first)
	}
	if !strings.Contains(first, `"isError":true`) {
		t.Errorf("the panicking tool did not produce an is_error result the model can see; got:\n%s", first)
	}
	if !strings.Contains(first, "tool exploded inside a real turn") {
		t.Errorf("the tool result does not carry the panic value; got:\n%s", first)
	}

	// Still alive: a contained tool panic must not end the conversation.
	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"second"}` + "\n")); err != nil {
		t.Fatalf("write second prompt: %v", err)
	}
	second := strings.Join(decodeUntilWithTimeout(t, dec, "agent_end", 15*time.Second), "\n")
	if !strings.Contains(second, "the fake reply") {
		t.Errorf("the child did not serve a second prompt after a contained tool panic; got:\n%s", second)
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	if code, sig := r.Wait(); code != 0 || sig != "" {
		t.Errorf("Wait() = (%d, %q), want (0, \"\") - a contained tool panic is not a child failure", code, sig)
	}
}

// panickingSender explodes on every send, standing in for the SDK sharp edges
// the turn goroutine actually walks (acc.Accumulate / MapAssistantMessage over
// model-supplied bytes). It panics on the turn goroutine, which is what makes
// it the right injection point for the worker-path containment.
type panickingSender struct{}

func (panickingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	panic("sdk exploded mid-turn")
}

// panickingTurnBuildFunc wires an engine whose every turn panics on the worker
// goroutine.
func panickingTurnBuildFunc() BuildFunc {
	return func(ctx context.Context, fe *fundi.Frontend, ro fundi.RuntimeOptions) (*fundi.Engine, func(), error) {
		client, err := llm.NewClient(
			llm.WithUpstream(llm.UpstreamAnthropic, panickingSender{}),
			llm.WithDefaultModel("claude-x"))
		if err != nil {
			return nil, nil, err
		}
		eng, err := fundi.NewEngine(fundi.EngineConfig{
			Client:   client,
			Tools:    &blockingToolSet{started: make(chan struct{})},
			Provider: "anthropic",
			ModelID:  "claude-x",
			Name:     "w1",
			BaseCtx:  ctx,
			OnFatal:  ro.OnFatal,
			ConvOpts: []llm.ConvOption{llm.NewConversation("fundi", "agent")},
		}, fe)
		if err != nil {
			return nil, nil, err
		}
		return eng, func() {}, nil
	}
}

// TestRunnerPanicInTurnWorkerEndsTheChild covers the other half of the
// containment story. A panic on the engine's turn worker is NOT on run()'s
// goroutine, so run()'s recover cannot see it; fundi.Engine.runTurnGuarded
// catches it and routes it back through EngineConfig.OnFatal.
//
// The requirement is specifically that this ends the CHILD rather than
// silently stopping the queue: stdout must reach a clean EOF (so the daemon's
// readStdout runs its ordinary child-exit path) and the exit code must be
// non-zero. A silently stopped queue would instead leave the child looking
// healthy forever while every prompt vanished.
func TestRunnerPanicInTurnWorkerEndsTheChild(t *testing.T) {
	r := New(Options{
		ChildID: "c_workerpanic",
		Parent:  t.Context(),
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
		Build:   panickingTurnBuildFunc(),
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

	// Deliberately never close stdin: the child must end on its own. If the
	// panic only stopped the queue, this read would block until the test
	// deadline instead of returning.
	type readResult struct {
		frames []byte
		err    error
	}
	done := make(chan readResult, 1)
	go func() {
		b, rerr := io.ReadAll(stdout)
		done <- readResult{frames: b, err: rerr}
	}()

	var got readResult
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stdout never reached EOF after a turn-worker panic; the child is wedged, not ended")
	}
	if got.err != nil {
		t.Errorf("stdout must EOF cleanly so the daemon runs its normal child-exit path, got: %v", got.err)
	}
	if !strings.Contains(string(got.frames), "agent_error") {
		t.Errorf("no agent_error frame explaining why the child died; got:\n%s", got.frames)
	}

	code, sig := r.Wait()
	if code == 0 {
		t.Error("exit code = 0 after a turn-worker panic, want non-zero")
	}
	if sig != "" {
		t.Errorf("signal = %q, want empty - a panic is an exit, not a signal", sig)
	}

	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close stdin: %v", err)
	}
}

// TestKillReportsTheSameExitShapeAsASignalledSubprocess pins the two halves of
// the Runner seam to ONE exit contract for the same user action.
//
// Task 2 went out of its way to preserve the subprocess contract — a
// signal-terminated child reports ExitCode 0 with the signal name, explicitly
// not -1 (see internal/child/runner.go). An escalated Kill of an in-process
// agent child used to report exit 1 with no signal instead, because Kill
// closes stdinR and Frontend.Run then surfaces os.ErrClosed rather than EOF.
// That put two different answers on the same wire fields
// (ChildSummary.ExitCode/ExitSignal, KillResponseData.Signal, meta.json)
// depending on which runner happened to be serving the child.
//
// The subprocess half is exercised through child.Spawn with no injected
// Runner, so it really is processRunner producing the reference values rather
// than this test asserting a remembered literal.
func TestKillReportsTheSameExitShapeAsASignalledSubprocess(t *testing.T) {
	// --- in-process half ---------------------------------------------------
	started := make(chan struct{})
	r := New(Options{
		ChildID: "c_killshape",
		Parent:  t.Context(),
		Runtime: fundi.RuntimeOptions{Cwd: t.TempDir()},
		Build:   blockingBuildFunc(started, writeFakeTurns(t, sampleToolUseResp)),
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Errorf("close stderr: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// The read end is closed by Kill while this is in flight, so an error
		// here is expected and not worth asserting on.
		_, _ = io.Copy(io.Discard, stdout)
	}()

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"go"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started; the turn never went in flight")
	}

	if err := r.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	inCode, inSignal := r.Wait()
	<-drained
	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close stdin: %v", err)
	}

	// --- subprocess half ---------------------------------------------------
	// A shell that ignores SIGTERM and never reads stdin, so the daemon's
	// shutdown ladder is forced all the way down to SIGKILL: closing stdin
	// does nothing, SIGTERM is trapped away, only SIGKILL ends it.
	ch, err := child.Spawn(t.Context(), child.SpawnSpec{
		ChildID:  "c_killshape_proc",
		Cwd:      t.TempDir(),
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", `trap "" TERM; while :; do sleep 0.05; done`},
	})
	if err != nil {
		t.Fatalf("spawn subprocess child: %v", err)
	}
	res, err := ch.Shutdown(300*time.Millisecond, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("subprocess Shutdown: %v", err)
	}
	if !res.Escalated {
		t.Fatal("the subprocess exited before SIGKILL; this test needs a signalled reference, not a clean exit")
	}

	if inCode != res.ExitCode || inSignal != res.Signal {
		t.Errorf("kill exit shape disagrees between runners:\n  in-process: (%d, %q)\n  subprocess: (%d, %q)",
			inCode, inSignal, res.ExitCode, res.Signal)
	}
	if inSignal != killedSignal {
		t.Errorf("in-process kill signal = %q, want %q", inSignal, killedSignal)
	}
}
