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
	silenceSlog(t) // the engine logs turn lifecycle at info; keep test output pristine
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, scriptedSender(t, bodies...)),
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
