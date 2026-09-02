package fundi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// sampleEndTurn is the plain end_turn companion to emit_test.go's sampleResp:
// together they script a two-iteration tool-use loop (tool_use, then done).
const sampleEndTurn = `{
 "id":"msg_2","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"done"}],
 "usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// syncBuffer is a bytes.Buffer safe for the cross-goroutine writes Frontend.Emit
// performs from the engine's turn goroutine while the test reads the output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// scriptedSender writes bodies (pretty-printed JSON anthropic.Message values)
// as one ndjson file and loads it through LoadFakeSender, so the production
// replay seam is exercised by every engine test.
func scriptedSender(t *testing.T, bodies ...string) llm.Sender {
	t.Helper()
	var lines []string
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
	s, err := LoadFakeSender(path)
	if err != nil {
		t.Fatalf("LoadFakeSender: %v", err)
	}
	return s
}

// fakeToolSet is a minimal agentloop.ToolSet standing in for the real registry
// (Task 8): one no-schema definition per key, name-sorted for cache stability.
type fakeToolSet map[string]func(ctx context.Context, in json.RawMessage) (string, error)

func (ts fakeToolSet) Definitions() []anthropic.ToolUnionParam {
	names := make([]string, 0, len(ts))
	for n := range ts {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]anthropic.ToolUnionParam, 0, len(names))
	for _, n := range names {
		defs = append(defs, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        n,
			InputSchema: anthropic.ToolInputSchemaParam{Type: "object"},
		}})
	}
	return defs
}

func (ts fakeToolSet) Execute(ctx context.Context, name string, in json.RawMessage) (string, error) {
	fn, ok := ts[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return fn(ctx, in)
}

// frameTypes parses ndjson frames and returns their "type" fields in order.
func frameTypes(t *testing.T, out string) []string {
	t.Helper()
	var types []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var f struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		types = append(types, f.Type)
	}
	return types
}

func assertFrameTypes(t *testing.T, out string, want []string) {
	t.Helper()
	got := frameTypes(t, out)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frame sequence:\n got %v\nwant %v", got, want)
	}
}

// newTestEngine builds an engine over a store-less (in-memory) rafiki
// conversation driven by a scripted sender.
func newTestEngine(t *testing.T, ts fakeToolSet, bodies ...string) (*Engine, *syncBuffer) {
	t.Helper()
	return newTestEngineWithSender(t, ts, scriptedSender(t, bodies...))
}

// newTestEngineWithSender is newTestEngine's building block for tests that
// need a Sender other than a plain scripted replay — e.g. one that pauses a
// specific call to pin down timing that a scripted-body-only test can't
// control deterministically.
func newTestEngineWithSender(t *testing.T, ts fakeToolSet, sender llm.Sender) (*Engine, *syncBuffer) {
	t.Helper()
	silenceSlog(t) // the engine logs turn lifecycle at info; keep test output pristine
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		Tools:    ts,
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	fe.handler = eng
	return eng, out
}

// newTestEngineWithConfig is newTestEngineWithSender's building block for
// tests that need an EngineConfig field neither of the other two helpers
// expose (OnTurnEnded, MaxCost). extra is applied to the config after every
// other field is set, so it can only add, never has to fight a default.
func newTestEngineWithConfig(t *testing.T, ts fakeToolSet, sender llm.Sender, extra func(*EngineConfig)) (*Engine, *syncBuffer) {
	t.Helper()
	silenceSlog(t)
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	cfg := EngineConfig{
		Client:   client,
		Tools:    ts,
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
	}
	if extra != nil {
		extra(&cfg)
	}
	eng, err := NewEngine(cfg, fe)
	if err != nil {
		t.Fatal(err)
	}
	fe.handler = eng
	return eng, out
}

func TestOnTurnEndedReportsCleanCompletion(t *testing.T) {
	var got []TurnOutcome
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	eng, _ := newTestEngineWithConfig(t, ts, scriptedSender(t, sampleResp, sampleEndTurn), func(cfg *EngineConfig) {
		cfg.OnTurnEnded = func(o TurnOutcome) { got = append(got, o) }
	})

	eng.HandlePrompt("go")
	eng.Wait()

	if len(got) != 1 {
		t.Fatalf("OnTurnEnded fired %d times, want 1: %+v", len(got), got)
	}
	if !got[0].Clean || got[0].LimitReason != "" || got[0].Err != nil {
		t.Errorf("outcome = %+v, want Clean with nothing else set", got[0])
	}
}

// TestOnTurnEndedReportsGuardrailReason is written but left empty-bodied
// (skipped) here: it needs Task 7's Config.MaxCost, which does not exist
// yet. Task 7's own Step 1 replaces this body with the real test — do not
// implement it in this task.
func TestOnTurnEndedReportsGuardrailReason(t *testing.T) {
	t.Skip("needs Task 7's EngineConfig.MaxCost — implemented there")
}

func TestOnTurnEndedReportsRealError(t *testing.T) {
	var got []TurnOutcome
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	// Only one scripted body: the loop's second Continue call finds the fake
	// sender exhausted and fails the turn outright — the same pattern
	// TestEngineEmitsAgentErrorOnLoopFailure already uses for a real (not
	// aborted) turn failure.
	eng, _ := newTestEngineWithConfig(t, ts, scriptedSender(t, sampleResp), func(cfg *EngineConfig) {
		cfg.OnTurnEnded = func(o TurnOutcome) { got = append(got, o) }
	})

	eng.HandlePrompt("go")
	eng.Wait()

	if len(got) != 1 || got[0].Clean {
		t.Fatalf("outcome = %+v, want a non-clean error outcome", got)
	}
	if got[0].Err == nil || !strings.Contains(got[0].Err.Error(), "scripted turns exhausted") {
		t.Errorf("Err = %v, want it to wrap the fake sender's exhaustion error", got[0].Err)
	}
}

// blockingSender wraps a Sender and pauses its Nth call until the test
// releases it. It exists to land a steer strictly after the agent loop's
// FINAL PendingUser poll but before agentloop.Run returns — a window a
// blocked tool can't reach, because PendingUser drains whatever is in
// steerBuf at call time, so anything buffered before it fires is captured
// in-turn (via drainSteers), never orphaned. The last PendingUser poll of a
// turn runs synchronously immediately before the Continue call that follows
// it, so pausing that Continue call (here, via the underlying Sender) is the
// only deterministic way to hold the loop open in the "late" window.
type blockingSender struct {
	inner llm.Sender

	mu      sync.Mutex
	n       int
	blockAt int
	started chan struct{}
	release chan struct{}
}

func (s *blockingSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.mu.Lock()
	s.n++
	hit := s.n == s.blockAt
	s.mu.Unlock()
	if hit {
		close(s.started)
		<-s.release
	}
	return s.inner.New(ctx, params)
}

// userMessageTexts returns, in order, the Content of every user-role
// message_start frame — the prompts/steers a run actually echoed to the TUI.
func userMessageTexts(t *testing.T, out string) []string {
	t.Helper()
	var texts []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		if f.Type != "message_start" || f.Message.Role != "user" {
			continue
		}
		var text string
		if err := json.Unmarshal(f.Message.Content, &text); err != nil {
			t.Fatalf("user message_start content not a string: %q: %v", l, err)
		}
		texts = append(texts, text)
	}
	return texts
}

func TestEngineRunsScriptedTurn(t *testing.T) {
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	eng, out := newTestEngine(t, ts, sampleResp, sampleEndTurn)

	eng.HandlePrompt("go")
	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo, before agent_start
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 1 (tool_use)
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_update", "message_end", // assistant turn 2 (end_turn)
		"agent_end", "agent_settled"})

	if got := eng.State().SessionID; got == "" {
		t.Fatal("State().SessionID is empty; the daemon sniffs it from get_state")
	}
	if got := eng.State().ModelID; got != "claude-x" {
		t.Fatalf("State().ModelID = %q, want claude-x", got)
	}
	if got := eng.State().Provider; got != "anthropic" {
		t.Fatalf("State().Provider = %q, want anthropic", got)
	}
	if got := eng.State().SessionName; got != "w1" {
		t.Fatalf("State().SessionName = %q, want w1", got)
	}
}

// TestHandlePromptReturnsWhileTurnInFlight guards the single most important
// property of the Handler contract: Frontend.Run dispatches inbound frames
// synchronously in its reader loop, so a HandlePrompt that blocked for the
// duration of a turn would stop the loop from ever reading an "abort" frame.
func TestHandlePromptReturnsWhileTurnInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		close(started)
		<-release
		return "file.txt", nil
	}}
	eng, out := newTestEngine(t, ts, sampleResp, sampleEndTurn, sampleEndTurn)

	eng.HandlePrompt("go")
	<-started // turn 1 is now parked inside the tool

	returned := make(chan struct{})
	go func() {
		eng.HandlePrompt("second")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("HandlePrompt blocked while a turn was in flight; in-band abort would be broken")
	}

	close(release)
	eng.Wait()

	// Both turns ran, in order, each fully bracketed by agent_start/agent_settled.
	types := frameTypes(t, out.String())
	var settled, starts int
	for _, ty := range types {
		switch ty {
		case "agent_settled":
			settled++
		case "agent_start":
			starts++
		}
	}
	if starts != 2 || settled != 2 {
		t.Fatalf("got %d agent_start / %d agent_settled, want 2/2: %v", starts, settled, types)
	}
	if types[len(types)-1] != "agent_settled" {
		t.Fatalf("last frame = %q, want agent_settled: %v", types[len(types)-1], types)
	}
}

// TestEngineRequeuesLateSteersAsOneTurn covers runTurn's own invention (the
// orphaned-steer requeue) and the fix to it: a steer that lands after the
// loop's LAST PendingUser poll must still surface, as exactly one extra
// bracketed turn — and when multiple steers land late, they must be joined
// into that ONE turn (mirroring drainSteers), not replayed as N separate
// turns.
func TestEngineRequeuesLateSteersAsOneTurn(t *testing.T) {
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	bs := &blockingSender{
		// Body 1: tool_use (the turn's only tool iteration; its PendingUser
		// poll fires here, before body 2 is requested, and finds steerBuf
		// empty). Body 2: end_turn, the call we pause. Body 3: end_turn for
		// the requeued turn.
		inner:   scriptedSender(t, sampleResp, sampleEndTurn, sampleEndTurn),
		blockAt: 2,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	eng, out := newTestEngineWithSender(t, ts, bs)

	eng.HandlePrompt("go")
	<-bs.started // the end_turn call is blocked; PendingUser already ran and
	// found nothing buffered, so anything queued now is provably late.

	eng.HandleSteer("line one")
	eng.HandleSteer("line two")
	close(bs.release)

	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo: "go"
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 1 (tool_use)
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_update", "message_end", // assistant turn 1 (end_turn)
		"agent_end", "agent_settled",
		"message_start", "message_end", // user echo: requeued, joined steer
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 2 (end_turn)
		"agent_end", "agent_settled",
	})

	texts := userMessageTexts(t, out.String())
	if len(texts) != 2 {
		t.Fatalf("got %d user-echoed messages, want 2 (prompt + one joined requeue): %v", len(texts), texts)
	}
	if texts[0] != "go" {
		t.Fatalf("first user message = %q, want %q", texts[0], "go")
	}
	if want := "line one\nline two"; texts[1] != want {
		t.Fatalf("requeued turn's user message = %q, want %q (one turn, both lines joined)", texts[1], want)
	}
}

// TestEngineEmitsAgentErrorOnLoopFailure covers the brief-mandated
// agent_error emission: when agentloop.Run fails outright (not via abort),
// the engine must emit an agent_error frame between the turn's last frame
// and agent_end rather than silently dropping the failure.
func TestEngineEmitsAgentErrorOnLoopFailure(t *testing.T) {
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	// Only one scripted body (the tool_use turn): the loop's second Continue
	// call finds the fake sender exhausted and fails the turn outright.
	eng, out := newTestEngine(t, ts, sampleResp)

	eng.HandlePrompt("go")
	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 1 (tool_use)
		"tool_execution_start", "tool_execution_end",
		"agent_error",
		"agent_end", "agent_settled",
	})

	var errFrame struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-3]), &errFrame); err != nil {
		t.Fatalf("parse agent_error frame: %v", err)
	}
	if errFrame.Type != "agent_error" {
		t.Fatalf("frame type = %q, want agent_error", errFrame.Type)
	}
	if !strings.Contains(errFrame.Error, "scripted turns exhausted") {
		t.Fatalf("agent_error.error = %q, want it to mention the exhausted fake sender", errFrame.Error)
	}
}

func TestLoadFakeSenderReplaysInOrderThenErrors(t *testing.T) {
	s := scriptedSender(t, sampleResp, sampleEndTurn)
	first, err := s.New(context.Background(), anthropic.MessageNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "msg_1" {
		t.Fatalf("first replayed message id = %q, want msg_1", first.ID)
	}
	second, err := s.New(context.Background(), anthropic.MessageNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != "msg_2" {
		t.Fatalf("second replayed message id = %q, want msg_2", second.ID)
	}
	if _, err := s.New(context.Background(), anthropic.MessageNewParams{}); err == nil {
		t.Fatal("exhausted scripted sender returned no error")
	}
}

func TestLoadFakeSenderRejectsBadFile(t *testing.T) {
	if _, err := LoadFakeSender(filepath.Join(t.TempDir(), "nope.ndjson")); err == nil {
		t.Fatal("missing file returned no error")
	}
	path := filepath.Join(t.TempDir(), "bad.ndjson")
	if err := os.WriteFile(path, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFakeSender(path); err == nil {
		t.Fatal("malformed body returned no error")
	}
}

// blockOnCtxTool returns a fake tool function that signals started, then
// blocks until its own context is cancelled and reports the cancellation as
// an error result — the fixture shared by every "abort lands mid-tool" test
// below. A real tool observes cancellation and returns promptly (see the
// bash tool's process-group kill); this stands in for that without a real
// subprocess.
func blockOnCtxTool(started chan struct{}) func(ctx context.Context, in json.RawMessage) (string, error) {
	return func(ctx context.Context, in json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
}

// ctxCheckingSender fails fast on an already-cancelled context before
// delegating, mirroring the real Anthropic HTTP client's ctx-awareness — the
// plain fakeSender ignores ctx entirely, which would let a turn's Continue
// call silently succeed via the replay script even after HandleAbort landed.
// Failing without delegating also means it does NOT consume a scripted body,
// so a later turn can still replay it.
type ctxCheckingSender struct{ inner llm.Sender }

func (s ctxCheckingSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.inner.New(ctx, params)
}

// TestEngineAbortMidTurnEndsCleanlyAndStaysReusable covers in-band abort end
// to end at the Engine layer: HandleAbort while a tool is genuinely blocked
// must (1) unblock the tool via ctx cancellation, (2) end the turn with
// tool_execution_end(isError) -> agent_end -> agent_settled and no
// agent_error frame (an abort is not a loop failure), and (3) leave the
// engine fully usable for the very next prompt — the entire point of
// in-band abort over Claude Code's SIGINT-and-respawn.
func TestEngineAbortMidTurnEndsCleanlyAndStaysReusable(t *testing.T) {
	started := make(chan struct{})
	ts := fakeToolSet{"bash": blockOnCtxTool(started)}
	sender := ctxCheckingSender{inner: scriptedSender(t, sampleResp, sampleEndTurn)}
	eng, out := newTestEngineWithSender(t, ts, sender)

	eng.HandlePrompt("go")
	<-started // the tool is now blocked on its ctx; the turn is genuinely in flight

	eng.HandleAbort()
	eng.Wait()

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo: "go"
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 1 (tool_use)
		"tool_execution_start", "tool_execution_end", // isError, checked below
		"agent_end", "agent_settled",
	})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var endFrame struct {
		Type    string `json:"type"`
		IsError bool   `json:"isError"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-3]), &endFrame); err != nil {
		t.Fatalf("parse tool_execution_end frame: %v", err)
	}
	if endFrame.Type != "tool_execution_end" || !endFrame.IsError {
		t.Fatalf("tool_execution_end frame = %+v, want isError true", endFrame)
	}

	// The abort must not have poisoned the sender's replay position: the
	// second turn's Continue call reaches the still-unconsumed sampleEndTurn
	// body and finishes with a clean end_turn — no agent_error.
	eng.HandlePrompt("second")
	eng.Wait()

	allTypes := frameTypes(t, out.String())
	secondTurn := allTypes[len(allTypes)-8:]
	want := []string{
		"message_start", "message_end", // user echo: "second"
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn (end_turn)
		"agent_end", "agent_settled",
	}
	if strings.Join(secondTurn, ",") != strings.Join(want, ",") {
		t.Fatalf("second turn's frames:\n got %v\nwant %v (a clean run, no agent_error)", secondTurn, want)
	}
}

// TestEngineAbortRepairsOrphanedToolUse is the wiring test the review found
// missing: every other abort test reaches RepairOrphans as a guaranteed
// no-op, because the fake tool persists its own ctx.Err() result before the
// cancelled branch runs (store-less conversations ignore ctx entirely — see
// orphans.go's doc comment). Deleting the RepairOrphans call from
// engine.go's cancelled-run branch must fail THIS test.
//
// It pre-seeds the conversation so the trailing assistant message already
// carries an unresolved tool_use (tu_1) before the turn under test even
// starts. The turn's own write-ahead user append (agentloop.Run's
// AppendUser of the prompt text) carries no tool_result blocks, so it
// doesn't resolve tu_1; and the turn's first (and only) Continue call is the
// one that observes the abort and fails, so drive() returns before ever
// extracting a tool_use batch to execute. tu_1 is therefore still genuinely
// unresolved when runTurn's cancelled branch fires, and RepairOrphans' real
// effect — a synthetic tool_result row — is asserted directly off
// conv.History, not via a call-counting spy.
func TestEngineAbortRepairsOrphanedToolUse(t *testing.T) {
	ts := fakeToolSet{} // never invoked: the turn's Continue call fails on
	// the cancelled ctx before drive() reaches a tool_use batch.
	sender := &blockingSender{
		// ctxCheckingSender fails a cancelled-ctx call before delegating, so
		// once released after HandleAbort the blocked Continue call returns
		// context.Canceled without consuming the filler scripted body.
		inner:   ctxCheckingSender{inner: scriptedSender(t, sampleEndTurn)},
		blockAt: 1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	eng, _ := newTestEngineWithSender(t, ts, sender)

	ctx := context.Background()
	if err := eng.conv.SeedHistory(ctx, []llm.Message{
		{Role: anthropic.MessageParamRoleUser, Content: llm.UserText("seed")},
		{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("tu_1", map[string]any{}, "bash"),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	eng.HandlePrompt("go")
	<-sender.started // the turn's only Continue call is now blocked — abort
	// lands strictly before any tool_use batch could be executed.

	eng.HandleAbort()
	close(sender.release)
	eng.Wait()

	history, err := eng.conv.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := history[len(history)-1]
	if last.Param.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("trailing history row role = %v, want user (RepairOrphans' synthesized row)", last.Param.Role)
	}
	if len(last.Param.Content) != 1 {
		t.Fatalf("trailing row has %d blocks, want 1 synthesized tool_result", len(last.Param.Content))
	}
	tr := last.Param.Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "tu_1" {
		t.Fatalf("trailing block = %+v, want a tool_result for tu_1", last.Param.Content[0])
	}
	if !tr.IsError.Value {
		t.Fatal("synthesized tool_result is not marked IsError")
	}
	if len(tr.Content) != 1 || tr.Content[0].OfText == nil ||
		tr.Content[0].OfText.Text != "Tool execution aborted by user." {
		t.Fatalf("synthesized tool_result content = %+v, want the standard abort text", tr.Content)
	}
}

// writeFrame marshals v as one ndjson line and writes it to w, bounded by a
// timeout so a stuck reader loop fails the test instead of hanging it.
func writeFrame(t *testing.T, w io.Writer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	done := make(chan error, 1)
	go func() {
		_, werr := w.Write(b)
		done <- werr
	}()
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("write frame %s: %v", v, werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write frame timed out; Frontend.Run's reader loop appears stuck")
	}
}

// TestFrontendDispatchesAbortFrameToInFlightTurn is Task 10's Requirement 4:
// the end-to-end contract no other test proves. Every other abort test calls
// eng.HandleAbort() directly from the test goroutine, which only guards the
// Handler-contract PRECONDITION (HandlePrompt returns promptly). This test
// drives a real "abort" ndjson frame down a real Frontend.Run's stdin reader
// loop while a turn is genuinely blocked in a tool, and proves the reader
// loop actually read and dispatched it — the only real evidence that in-band
// abort works over the wire, not just inside the Engine's own API.
func TestFrontendDispatchesAbortFrameToInFlightTurn(t *testing.T) {
	silenceSlog(t)
	started := make(chan struct{})
	ts := fakeToolSet{"bash": blockOnCtxTool(started)}
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", scriptedSender(t, sampleResp)),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	out := &syncBuffer{}
	fe := NewFrontend(inR, out, nil)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		Tools:    ts,
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	fe.handler = eng

	runDone := make(chan error, 1)
	go func() { runDone <- fe.Run() }()

	writeFrame(t, inW, map[string]any{"type": "prompt", "message": "go"})
	<-started // proves Run's reader loop already read AND dispatched the prompt
	// frame (HandlePrompt queued it and the worker reached the blocking tool)
	// before we send the next line.

	writeFrame(t, inW, map[string]any{"type": "abort"})

	waitDone := make(chan struct{})
	go func() {
		eng.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("engine never finished its turn after an abort frame was written to stdin; " +
			"Frontend.Run's reader loop did not dispatch it")
	}

	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Frontend.Run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Frontend.Run did not return after stdin closed")
	}

	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo: "go"
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn (tool_use)
		"tool_execution_start", "tool_execution_end", // isError: the tool observed the abort
		"agent_end", "agent_settled",
	})
}
