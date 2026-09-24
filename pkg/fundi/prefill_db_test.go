// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

// prefillGateSender wraps a scripted replay and FAILS THE TEST if the model
// is called while closed. The startup paths this file pins (a pre-fill-shaped
// history must never reach Resume; a partial r1 completion must make no LLM
// call) are enforced by the gate itself, not by a count compared after the
// fact: any premature call fails the test the moment it happens.
type prefillGateSender struct {
	t     *testing.T
	inner *capturingSender

	mu   sync.Mutex
	open bool
}

func (s *prefillGateSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.mu.Lock()
	if !s.open {
		s.mu.Unlock()
		s.t.Error("prefill: LLM called before any prompt arrived")
		return nil, errors.New("prefill gate closed")
	}
	s.mu.Unlock()
	return s.inner.New(ctx, params)
}

// openGate lets calls through; opened only once a prompt has been queued.
func (s *prefillGateSender) openGate() {
	s.mu.Lock()
	s.open = true
	s.mu.Unlock()
}

// callCount reports how many requests the inner sender actually served.
func (s *prefillGateSender) callCount() int { return s.inner.callCount() }

// prefillDBEngine builds an engine over a DB-backed conversation reattached
// by external ref, with the shared wiring every test in this file needs.
func prefillDBEngine(t *testing.T, pool *pgxpool.Pool, sender llm.Sender, ref string, extra func(*EngineConfig)) (*Engine, *syncBuffer) {
	t.Helper()
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	cfg := EngineConfig{
		Client:   client,
		Tools:    prefillFakeRead(nil),
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
	}
	if extra != nil {
		extra(&cfg)
	}
	eng, err := NewEngine(cfg, fe)
	if err != nil {
		t.Fatal(err)
	}
	return eng, out
}

// prefillDBSeedClient opens a seeding handle on the same pool.
func prefillDBSeedClient(t *testing.T, pool *pgxpool.Pool) *llm.Client {
	t.Helper()
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", newCapturingSender(t, sampleEndTurn)),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// prefillSeedRows builds the r0/r1/r2 history rows a pre-fill persists, with
// path in every read input.
func prefillSeedRows(path string) []llm.Message {
	r0 := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(PrefillPreamble)},
	}
	input, _ := json.Marshal(prefillReadInput{Path: path, Offset: 1, Limit: prefillOpenLimit})
	r1 := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("prefill_0001", json.RawMessage(input), "read"),
		},
	}
	r2 := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolResultBlock("prefill_0001", path+" body", false),
		},
	}
	return []llm.Message{r0, r1, r2}
}

// waitForPrefillDBRows polls the DB-backed history until it reaches wantRows
// rows (concurrent-safe through pgx), bailing early on an agent_error frame.
func waitForPrefillDBRows(t *testing.T, eng *Engine, out *syncBuffer, wantRows int) []store.Message {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		hist, err := eng.conv.History(context.Background())
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		if len(hist) >= wantRows {
			return hist
		}
		if msg := out.String(); strings.Contains(msg, "agent_error") {
			t.Fatalf("prefill emitted agent_error instead of reaching %d rows: %s", wantRows, msg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("history never reached %d rows (have %d)", wantRows, len(hist))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPrefillResumeDoesNotContinueCompletePrefill pins the defensive rule: a
// history that is exactly a complete pre-fill (r0, r1, r2 — its tail is user
// tool_results) must NEVER reach agentloop.Resume, whether or not the
// pre-fill field survived the restart. Resume would Continue, calling the
// model with the files and no task.
func TestPrefillResumeDoesNotContinueCompletePrefill(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) { testPrefillCompleteTailSkipsResume(t, nil) })
	t.Run("configured", func(t *testing.T) {
		testPrefillCompleteTailSkipsResume(t, []protocol.PrefillRead{{Path: "/tmp/a.txt"}})
	})
}

func testPrefillCompleteTailSkipsResume(t *testing.T, prefill []protocol.PrefillRead) {
	t.Helper()
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	ref := "prefill-complete-" + t.Name()

	// Seed r0/r1/r2 directly, the way a previous process's pre-fill left it.
	seedClient := prefillDBSeedClient(t, pool)
	conv, err := seedClient.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := conv.SeedHistory(ctx, prefillSeedRows("/tmp/a.txt")); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if hist, err := conv.History(ctx); err != nil || len(hist) != 3 {
		t.Fatalf("seeded history = %d rows (%v), want 3", len(hist), err)
	}

	gate := &prefillGateSender{t: t, inner: newCapturingSender(t, sampleEndTurn)}
	var eng *Engine
	var out *syncBuffer
	eng, out = prefillDBEngine(t, pool, gate, ref, func(cfg *EngineConfig) {
		cfg.AutoResume = true
		cfg.Prefill = prefill
		if len(prefill) > 0 {
			// runPrefill must NOT run either: the shape is already complete.
			cfg.Tools = failIfCalledToolSet(t, "prefill executed on a complete history")
		}
	})
	defer eng.Close()

	// Settle: the worker's startup classify runs against a 3-row history and
	// must do nothing — no Resume, no pre-fill, no LLM call. The gate makes
	// any premature call fail the test the moment it happens, so the sleep
	// only gates when the prompt below arrives, not whether we can detect a
	// violation.
	time.Sleep(300 * time.Millisecond)
	if gate.callCount() != 0 {
		t.Fatalf("LLM was called %d times before any prompt", gate.callCount())
	}
	if hist, err := eng.conv.History(ctx); err != nil || len(hist) != 3 {
		t.Fatalf("history after settle = %d rows (%v), want 3 unchanged", len(hist), err)
	}

	// Now the prompt: exactly one LLM call, task text last.
	gate.openGate()
	eng.HandlePrompt("do the task")
	eng.Wait()

	if gate.callCount() != 1 {
		t.Fatalf("sender served %d calls, want exactly 1 (the prompt turn)", gate.callCount())
	}
	params := gate.inner.lastParams(t)
	msgs := params.Messages
	if len(msgs) != 3 {
		t.Fatalf("request has %d messages, want 3", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last request message role = %v, want user", last.Role)
	}
	text := ""
	for _, b := range last.Content {
		if b.OfText != nil {
			text = b.OfText.Text
		}
	}
	if text != "do the task" {
		t.Fatalf("task text = %q, want it present (and last) in the merged message", text)
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillCompletesPartialR1: a process died after persisting r0+r1. The
// restarted engine (pre-fill configured + AutoResume) re-executes r1's
// inputs verbatim and writes r2 — with NO LLM call.
func TestPrefillCompletesPartialR1(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	ref := "prefill-partial-r1"

	seedClient := prefillDBSeedClient(t, pool)
	conv, err := seedClient.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// Seed only r0+r1: r2 is what the pre-fill must write.
	rows := prefillSeedRows("/tmp/a.txt")
	if err := conv.SeedHistory(ctx, rows[:2]); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	var mu sync.Mutex
	var readPaths []string
	ts := fakeToolSet{
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", err
			}
			mu.Lock()
			readPaths = append(readPaths, cmd.Path)
			mu.Unlock()
			return cmd.Path + " body", nil
		},
	}

	gate := &prefillGateSender{t: t, inner: newCapturingSender(t, sampleEndTurn)}
	eng, out := prefillDBEngine(t, pool, gate, ref, func(cfg *EngineConfig) {
		cfg.AutoResume = true
		cfg.Prefill = []protocol.PrefillRead{{Path: "/tmp/a.txt"}}
		cfg.Tools = ts
	})
	defer eng.Close()

	hist := waitForPrefillDBRows(t, eng, out, 3)
	if len(hist) != 3 {
		t.Fatalf("history has %d rows, want exactly 3", len(hist))
	}

	// r2 was written by executing r1's inputs verbatim.
	r1 := hist[1].Param.Content[0].OfToolUse
	r2 := hist[2].Param.Content[0].OfToolResult
	if r2.ToolUseID != r1.ID {
		t.Fatalf("r2 tool_use_id = %q, want r1's %q", r2.ToolUseID, r1.ID)
	}
	if !strings.HasPrefix(r1.ID, "prefill_") {
		t.Fatalf("r1 id %q lacks the prefill_ prefix", r1.ID)
	}
	if got := r2.Content[0].OfText.Text; got != "/tmp/a.txt body" {
		t.Fatalf("r2 result = %q, want the re-executed read output", got)
	}

	mu.Lock()
	if len(readPaths) != 1 || readPaths[0] != "/tmp/a.txt" {
		t.Fatalf("read calls = %v, want exactly r1's input path", readPaths)
	}
	mu.Unlock()

	if gate.callCount() != 0 {
		t.Fatalf("LLM was called %d times completing a partial r1, want 0", gate.callCount())
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillSyntheticRowUsageIsNull: the pre-fill's rows persist with NULL
// usage — "not reported", never zero. The prefix plus NULL usage is the
// provenance marker that distinguishes a pre-fill from a real turn.
func TestPrefillSyntheticRowUsageIsNull(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	ref := "prefill-usage-null"

	gate := &prefillGateSender{t: t, inner: newCapturingSender(t, sampleEndTurn)}
	eng, out := prefillDBEngine(t, pool, gate, ref, func(cfg *EngineConfig) {
		cfg.Prefill = []protocol.PrefillRead{{Path: "/tmp/a.txt"}}
	})
	defer eng.Close()

	hist := waitForPrefillDBRows(t, eng, out, 3)
	convID := eng.conv.ID
	for _, ordinal := range []int{0, 1, 2} {
		var inTok, outTok *int64
		if err := pool.QueryRow(ctx, `
			SELECT input_tokens, output_tokens
			  FROM conversations.conversation_message
			 WHERE conversation_id = $1::uuid AND ordinal = $2`, convID, ordinal).Scan(&inTok, &outTok); err != nil {
			t.Fatalf("query ordinal %d: %v", ordinal, err)
		}
		if inTok != nil || outTok != nil {
			t.Fatalf("ordinal %d has input_tokens=%v output_tokens=%v, want NULL (usage not reported)",
				ordinal, inTok, outTok)
		}
	}
	if len(hist) != 3 {
		t.Fatalf("history has %d rows, want 3", len(hist))
	}
	if hist[1].StopReason != "" {
		t.Fatalf("r1 stop reason = %q, want empty", hist[1].StopReason)
	}
	if gate.callCount() != 0 {
		t.Fatalf("LLM was called %d times, want 0", gate.callCount())
	}
}

// failIfCalledToolSet is a ToolSet whose definitions look complete (read and
// glob) but whose Execute fails the test if ever invoked.
func failIfCalledToolSet(t *testing.T, msg string) fakeToolSet {
	return fakeToolSet{
		"read": func(context.Context, json.RawMessage) (string, error) {
			t.Helper()
			t.Error(msg)
			return "", fmt.Errorf("%s", msg)
		},
		"glob": func(context.Context, json.RawMessage) (string, error) {
			t.Helper()
			t.Error(msg)
			return "", fmt.Errorf("%s", msg)
		},
	}
}
