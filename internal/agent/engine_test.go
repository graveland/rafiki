package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/rafiki/llm"
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
		llm.WithUpstream(llm.UpstreamAnthropic, sender),
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
		ConvOpts: []llm.ConvOption{llm.NewConversation("fundi", "agent")},
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	fe.handler = eng
	return eng, out
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
