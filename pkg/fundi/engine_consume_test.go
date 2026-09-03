package fundi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm"
)

// ackLog records the ids handed to EngineConfig.OnConsumed, in order. The
// engine calls OnConsumed from its turn worker while the test goroutine reads,
// so it needs its own lock.
type ackLog struct {
	mu  sync.Mutex
	ids []string
}

func (a *ackLog) record(ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids = append(a.ids, ids...)
}

func (a *ackLog) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ids...)
}

func (a *ackLog) joined() string { return strings.Join(a.snapshot(), ",") }

// newConsumingEngine mirrors newTestEngineWithSender's config and adds the two
// hooks these tests need. It builds the EngineConfig here rather than widening
// newTestEngineWithSender, whose ~20 callers have no use for either hook.
//
// KEEP IN SYNC with newTestEngineWithSender (engine_test.go): the EngineConfig
// below is a verbatim copy of that harness's plus OnConsumed/OnFatal, and a
// field added there will NOT reach these tests on its own.
func newConsumingEngine(t *testing.T, ts fakeToolSet, sender llm.Sender,
	onConsumed func([]string), onFatal func(error)) (*Engine, *syncBuffer) {
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
	eng, err := NewEngine(EngineConfig{
		Client:     client,
		Tools:      ts,
		Provider:   "anthropic",
		ModelID:    "claude-x",
		Name:       "w1",
		ConvOpts:   []llm.ConvOption{llm.NewConversation("", "agent")},
		OnConsumed: onConsumed,
		OnFatal:    onFatal,
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	fe.handler = eng
	return eng, out
}

// TestEngineAcksWhenAPromptEntersATurn pins the ack POINT, which is the whole
// design. Acking on receipt would be worthless: a message sits in e.pending
// for the length of whatever turn is ahead of it, and a fundi child runs
// in-process, so the daemon dying and the child dying are one event. The
// window this closes is a whole turn wide.
//
// Two prompts are queued while the first turn is blocked inside its LLM call.
// Only the first may be acked at that instant; the second has not entered a
// turn. The assertion is deterministic in the direction that matters: an
// ack-on-receipt implementation would have retired BOTH before the sender was
// ever reached.
func TestEngineAcksWhenAPromptEntersATurn(t *testing.T) {
	var acked ackLog
	bs := &blockingSender{
		inner:   scriptedSender(t, sampleEndTurn, sampleEndTurn),
		blockAt: 1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	eng, _ := newConsumingEngine(t, fakeToolSet{}, bs, acked.record, nil)

	eng.HandlePromptID("F1", "first")
	eng.HandlePromptID("F2", "second")

	<-bs.started // turn 1 is inside the LLM call, so its ack has already run
	if got := acked.joined(); got != "F1" {
		t.Fatalf("acked %q while the first turn is still running, want only F1 — "+
			"F2 is queued behind it and has not entered a turn", got)
	}

	close(bs.release)
	eng.Wait()
	if got := acked.joined(); got != "F1,F2" {
		t.Fatalf("acked %q after both turns ran, want F1,F2", got)
	}
}

// TestEngineAcksASteerWhenItIsInjected covers the second ack point: a steer is
// retired in drainSteers, as it is handed to the running turn — not when the
// frame arrives, which may be a whole tool call earlier.
func TestEngineAcksASteerWhenItIsInjected(t *testing.T) {
	var acked ackLog
	started := make(chan struct{})
	release := make(chan struct{})
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		close(started)
		<-release
		return "file.txt", nil
	}}
	eng, out := newConsumingEngine(t, ts,
		scriptedSender(t, sampleResp, sampleEndTurn), acked.record, nil)

	eng.HandlePromptID("F1", "go")
	<-started // parked inside the tool; the next PendingUser poll is still ahead

	eng.HandleSteerID("F2", "also this")
	if got := acked.joined(); got != "F1" {
		t.Fatalf("acked %q the moment the steer was buffered, want only F1 — "+
			"a buffered steer has not entered the turn yet", got)
	}

	close(release)
	eng.Wait()
	if got := acked.joined(); got != "F1,F2" {
		t.Fatalf("acked %q, want F1,F2: the steer must be retired when it is injected", got)
	}
	if texts := userMessageTexts(t, out.String()); len(texts) != 2 || texts[1] != "also this" {
		t.Fatalf("user messages = %v, want the steer to have been injected into the turn", texts)
	}
}

// TestEngineDoesNotAckWhatFatalDiscards is the inverse assertion, and it
// guards silent message loss. fatal() clears pending and steerBuf when the
// turn worker dies; nothing in either entered a turn, so those rows must stay
// unacked and replay against the replacement child. Acking them there would
// look like tidy bookkeeping and would delete work nobody ever did.
func TestEngineDoesNotAckWhatFatalDiscards(t *testing.T) {
	var acked ackLog
	fatalCalled := make(chan struct{})
	// The sender panics on the turn worker, which is the one path to fatal().
	// It stalls its own throwaway writer, not the Frontend's, so nothing here
	// blocks on stdout.
	eng, _ := newConsumingEngine(t, fakeToolSet{}, panickingSender{w: newStallWriter()},
		acked.record, func(error) { close(fatalCalled) })

	eng.HandlePromptID("F1", "first")
	eng.HandlePromptID("F2", "second")

	select {
	case <-fatalCalled:
	case <-time.After(20 * time.Second):
		t.Fatal("OnFatal was never called; the panicking turn did not reach fatal()")
	}
	eng.Wait()

	if got := acked.joined(); got != "F1" {
		t.Fatalf("acked %q, want only F1: F2 never entered a turn — fatal() discarded it, "+
			"and acking a discarded message deletes work that was never done", got)
	}
}

// TestEngineAcksSteersThatArriveDuringTheFinalCall covers the same fix as
// TestEngineFoldsSteersArrivingDuringTheFinalCallIntoTheSameTurn from the ack
// side: two steers buffered while the turn's last Continue call (end_turn) is
// still in flight must be retired via drainSteers's normal ack -- e.consume,
// called with every buffered id -- inside this SAME turn, not left unacked
// for a requeue that no longer happens.
func TestEngineAcksSteersThatArriveDuringTheFinalCall(t *testing.T) {
	var acked ackLog
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	bs := &blockingSender{
		// Body 1: tool_use (its PendingUser poll fires here and finds nothing).
		// Body 2: end_turn, the call we pause -- both steers land while it is
		// still in flight. Body 3: end_turn, the genuinely final reply.
		inner:   scriptedSender(t, sampleResp, sampleEndTurn, sampleEndTurn),
		blockAt: 2,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	eng, out := newConsumingEngine(t, ts, bs, acked.record, nil)

	eng.HandlePromptID("F1", "go")
	<-bs.started

	eng.HandleSteerID("F2", "line one")
	eng.HandleSteerID("F3", "line two")
	if got := acked.joined(); got != "F1" {
		t.Fatalf("acked %q, want only F1: two buffered steers are not yet injected", got)
	}
	close(bs.release)
	eng.Wait()

	if got := acked.joined(); got != "F1,F2,F3" {
		t.Fatalf("acked %q, want F1,F2,F3 -- both steer ids must be retired once "+
			"drainSteers injects them into the running turn", got)
	}
	texts := userMessageTexts(t, out.String())
	if want := []string{"go", "line one", "line two"}; !slices.Equal(texts, want) {
		t.Fatalf("user messages = %v, want %v", texts, want)
	}
}
