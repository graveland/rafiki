package fundi

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

const slashSummaryBody = `{"id":"msg_s","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn",
 "content":[{"type":"text","text":"<summary>handover</summary>"}],
 "usage":{"input_tokens":200,"output_tokens":20}}`

func compactionPolicyOpt(cfg *EngineConfig) {
	cfg.ConvOpts = append(cfg.ConvOpts, llm.WithCompaction(llm.CompactionPolicy{
		ContextWindowFn: func() int { return 1_000_000 },
	}))
}

func seedFourRows(t *testing.T, eng *Engine) {
	t.Helper()
	assert.NewAborting(t).NoError(eng.conv.SeedHistory(context.Background(), []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("a")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("b")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("c")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("d")),
	}))
}

func boundaryTriggers(sink *recordingSink) []string {
	var out []string
	for _, ev := range sink.events {
		if cb := ev.GetCompactionBoundary(); cb != nil {
			out = append(out, cb.GetTrigger())
		}
	}
	return out
}

// A /compact prompt compacts and a /clear prompt clears, both in queue order and
// neither as a turn: no agent_start, no assistant call beyond the summary.
func TestSlashCompactAndClearPromptsRunWithoutATurn(t *testing.T) {
	c := assert.NewCollecting(t)
	sink := &recordingSink{}
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, scriptedSender(t, slashSummaryBody), func(cfg *EngineConfig) {
		compactionPolicyOpt(cfg)
		cfg.NativeSink = sink
	})
	seedFourRows(t, eng)

	eng.HandlePrompt("/compact keep the schema")
	eng.HandlePrompt("/clear")
	eng.Wait()

	types := frameTypes(t, out.String())
	c.Require().GreaterOrEqual(0, indexOf(types, "compaction_end"), "no compaction frames; got %v", types)
	c.Eq(-1, indexOf(types, "agent_start"), "a slash command must not start a turn; got %v", types)
	c.EqDeep([]string{"manual", "clear"}, boundaryTriggers(sink), "native boundaries in order")

	hist, err := eng.conv.History(context.Background())
	c.Require().NoError(err)
	c.Require().Len(hist, 1, "working set after /clear")
	c.True(store.IsClearBoundary(hist[0]), "working set is the clear boundary")
}

// The daemon never classifies steer text, so the engine must not either: a steer
// that falls back to a prompt while idle runs as an ordinary turn.
func TestSlashSteerTextIsNotACommand(t *testing.T) {
	c := assert.NewCollecting(t)
	sink := &recordingSink{}
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, scriptedSender(t, sampleEndTurn), func(cfg *EngineConfig) {
		compactionPolicyOpt(cfg)
		cfg.NativeSink = sink
	})

	eng.HandleSteer("/clear")
	eng.Wait()

	types := frameTypes(t, out.String())
	c.GreaterOrEqual(0, indexOf(types, "agent_start"), "an idle steer must run as a turn; got %v", types)
	c.Empty(boundaryTriggers(sink), "a steer must never clear or compact")
}

// Orphaned steers are rejoined into one requeued prompt; that join must not be
// interpreted as a command either.
func TestSlashRequeuedSteersAreNotCommands(t *testing.T) {
	c := assert.NewCollecting(t)
	eng, _ := newTestEngineWithConfig(t, fakeToolSet{}, scriptedSender(t), nil)
	eng.requeueSteers([]queued{{ids: []string{"a"}, text: "/clear"}, {ids: []string{"b"}, text: "fix the tests"}})
	eng.mu.Lock()
	defer eng.mu.Unlock()
	c.Require().Len(eng.pending, 1, "pending after requeue")
	c.False(eng.pending[0].command, "a rejoined steer batch is not a command")
	c.EqDeep([]string{"a", "b"}, eng.pending[0].ids, "ids of every joined steer")
}

// A prompt carrying attachments is a literal turn, never a command.
func TestSlashWithAttachmentsIsNotACommand(t *testing.T) {
	c := assert.NewCollecting(t)
	eng, _ := newTestEngineWithConfig(t, fakeToolSet{}, scriptedSender(t), nil)
	c.False(eng.runSlash("/clear", []llm.UserImage{{MediaType: "image/png", Data: []byte("x")}}), "runSlash consumed an attachment prompt")
}

// /compact on a conversation with no compaction policy is reported, not silent.
func TestSlashCompactWithoutPolicyEmitsAgentError(t *testing.T) {
	c := assert.NewCollecting(t)
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, scriptedSender(t), nil)
	seedFourRows(t, eng)

	eng.HandlePrompt("/compact")
	eng.Wait()

	c.GreaterOrEqual(0, indexOf(frameTypes(t, out.String()), "agent_error"), "no agent_error frame:\n%s", out.String())
}

// ctxBlockingSender holds every call until its context is cancelled.
type ctxBlockingSender struct {
	started chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (s *ctxBlockingSender) New(ctx context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// An abort reaches a running /compact, and the engine carries on afterwards: no
// agent_error, and a steer that arrived meanwhile is requeued, not stranded.
func TestSlashCompactIsAbortable(t *testing.T) {
	c := assert.NewCollecting(t)
	bs := &ctxBlockingSender{started: make(chan struct{})}
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, bs, compactionPolicyOpt)
	seedFourRows(t, eng)

	eng.HandlePrompt("/compact")
	select {
	case <-bs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the summary call never started")
	}
	eng.HandleSteerID("S1", "while compacting")
	eng.mu.Lock()
	buffered := len(eng.steerBuf)
	eng.mu.Unlock()
	c.Eq(1, buffered, "a steer during /compact buffers like one during a turn")

	eng.HandleAbort()
	// The requeued steer becomes the next turn and reaches the sender (the
	// second call); the first call was the aborted summary.
	deadline := time.Now().Add(10 * time.Second)
	for bs.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("abort did not release /compact and requeue the steer as a turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Eq(-1, indexOf(frameTypes(t, out.String()), "agent_error"), "an abort is not an error:\n%s", out.String())
	eng.HandleAbort() // release the steer's turn so the engine can shut down
	eng.Wait()
}

func TestEndsAtBoundary(t *testing.T) {
	kind := func(k string) *string { return &k }
	user := func(s string) anthropic.MessageParam { return anthropic.NewUserMessage(anthropic.NewTextBlock(s)) }
	cases := []struct {
		name string
		h    []store.Message
		want bool
	}{
		{"empty", nil, false},
		{"ordinary tail", []store.Message{{Param: user("hi")}}, false},
		{"clear boundary", []store.Message{{Kind: kind(store.KindClear), Param: user(store.ClearBoundaryText)}}, true},
		{"claude clear row is a real message", []store.Message{{Kind: kind(store.KindClear), Param: user("real head")}}, false},
		{"compaction summary alone", []store.Message{{Kind: kind(store.KindCompactionSummary), Param: user("s")}}, true},
		{"summary then tail", []store.Message{
			{Kind: kind(store.KindCompactionSummary), Param: user("s")},
			{Kind: kind(store.KindCompactionTail), Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("t"))},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewCollecting(t).Eq(tc.want, endsAtBoundary(tc.h), "endsAtBoundary")
		})
	}
}

// After a /clear, a restart's auto-resume must not call the model: the working
// set is the boundary alone and there is nothing to answer.
func TestSlashStartupResumeSkipsAfterClear(t *testing.T) {
	c := assert.NewCollecting(t)
	silenceSlog(t)
	bs := &ctxBlockingSender{started: make(chan struct{})}
	client, err := llm.NewClient(llm.WithProviderSender("anthropic", bs), llm.WithDefaultModel("claude-x"))
	c.Require().NoError(err)
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	eng, err := NewEngine(EngineConfig{
		Client: client, Tools: fakeToolSet{}, Provider: "anthropic", ModelID: "claude-x", Name: "w1",
		ConvOpts:   []llm.ConvOption{llm.NewConversation("", "agent")},
		AutoResume: true,
	}, fe)
	c.Require().NoError(err)
	seedFourRows(t, eng)
	c.Require().NoError(eng.conv.Clear(context.Background()))

	eng.Start()
	eng.HandlePrompt("/clear") // a command makes no model call; it runs after startup resume
	eng.Wait()

	c.Eq(int32(0), bs.calls.Load(), "resume after /clear called the model")
	c.Eq(-1, indexOf(frameTypes(t, out.String()), "agent_start"), "resume after /clear started a turn")
}
