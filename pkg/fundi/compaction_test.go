package fundi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// newCompactionEmitter builds an Emitter over a syncBuffer frontend plus a
// recordingSink, the pair every emitter test asserts against.
func newCompactionEmitter(t *testing.T) (*Emitter, *syncBuffer, *recordingSink) {
	t.Helper()
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	em := NewEmitter(fe, "anthropic", nil)
	sink := &recordingSink{}
	em.SetNativeSink(sink)
	return em, out, sink
}

// TestCompactionEmitterFramesAndNativeEvent pins the successful boundary: a
// start/end frame pair in order, and exactly one native CompactionBoundary
// carrying the trigger and both token counts.
func TestCompactionEmitterFramesAndNativeEvent(t *testing.T) {
	c := assert.NewCollecting(t)
	em, out, sink := newCompactionEmitter(t)

	em.CompactionStart()
	em.CompactionEnd(llm.CompactionEvent{Trigger: "threshold", PreTokens: 900_000, PostTokens: 60_000, Succeeded: true})

	assertFrameTypes(t, out.String(), []string{"compaction_start", "compaction_end"})

	c.Require().Len(sink.events, 1, "native events")
	cb := sink.events[0].GetCompactionBoundary()
	c.Require().NotNil(cb, "event is not a CompactionBoundary")
	c.Eq("threshold", cb.GetTrigger(), "trigger")
	c.Eq(int32(900_000), cb.GetPreTokens(), "pre_tokens")
	c.Eq(int32(60_000), cb.GetPostTokens(), "post_tokens")
}

// TestCompactionEndWithoutPreTokensPublishesNoNativeEvent pins that a FAILED
// attempt (which wrote nothing) still emits both frames — keeping the status
// stack balanced — but records no boundary, because there is none.
func TestCompactionEndWithoutPreTokensPublishesNoNativeEvent(t *testing.T) {
	c := assert.NewCollecting(t)
	em, out, sink := newCompactionEmitter(t)

	em.CompactionStart()
	em.CompactionEnd(llm.CompactionEvent{Trigger: "overflow", PreTokens: 0, PostTokens: 0})

	assertFrameTypes(t, out.String(), []string{"compaction_start", "compaction_end"})
	c.Len(sink.events, 0, "a failed attempt must publish no native boundary; got %+v", sink.events)
}

// TestCompactionEndSucceededPublishesWithZeroPreTokens pins the gate: a
// SUCCESSFUL compaction whose sender reported a zero usage figure (a legitimate
// "not reported") still publishes the native boundary. Gating on PreTokens > 0
// would silently drop it.
func TestCompactionEndSucceededPublishesWithZeroPreTokens(t *testing.T) {
	c := assert.NewCollecting(t)
	em, _, sink := newCompactionEmitter(t)

	em.CompactionEnd(llm.CompactionEvent{Trigger: "threshold", PreTokens: 0, PostTokens: 0, Succeeded: true})

	c.Require().Len(sink.events, 1, "a successful compaction must publish its boundary even with zero reported tokens")
	cb := sink.events[0].GetCompactionBoundary()
	c.Require().NotNil(cb, "event is not a CompactionBoundary")
	c.Eq(int32(0), cb.GetPreTokens(), "pre_tokens")
}

// TestCompactionStatusPushesAndPops feeds the emitter's OWN frames through
// child.StateMachine: compaction_start must push `compacting` and
// compaction_end must pop back to the prior status, so the rail shows the
// compacting state exactly while it lasts. Pinning the exact frame strings here
// is the point — a rename in the emitter would silently stop moving the status.
func TestCompactionStatusPushesAndPops(t *testing.T) {
	c := assert.NewCollecting(t)
	em, out, _ := newCompactionEmitter(t)

	em.CompactionStart()
	em.CompactionEnd(llm.CompactionEvent{Trigger: "threshold", PreTokens: 1, PostTokens: 1})

	sm := child.NewStateMachine()
	sm.OnFirstResponse() // spawning -> idle
	c.Eq(protocol.StatusIdle, sm.Current(), "status before compaction")

	for _, ft := range frameTypes(t, out.String()) {
		sm.OnPiEvent(ft)
		switch ft {
		case "compaction_start":
			c.Eq(protocol.StatusCompacting, sm.Current(), "status on compaction_start")
		case "compaction_end":
			c.Eq(protocol.StatusIdle, sm.Current(), "status on compaction_end")
		}
	}
}

// TestCompactionContextWindowUnknownIsZero pins the deliberate 0: an unknown
// window must never trigger proactive compaction, so it must NOT fall back to
// prefillFallbackContext (which would summarise a healthy conversation at an
// invented size). A known model still answers with its real window.
func TestCompactionContextWindowUnknownIsZero(t *testing.T) {
	c := assert.NewCollecting(t)

	c.Eq(0, compactionContextWindow(nil, providers.KindAnthropic, "claude-x", 0), "nil client")

	// A seeded catalog is "fresh" (SeedForTest pins fetched), so querying a
	// model it does not carry answers ok=false without ever hitting the network.
	empty := routing.NewModelCatalog(nil, time.Hour, nil)
	empty.SeedForTest([]routing.CatalogEntry{{ID: "some-other-model", Created: 1}})
	unknown, err := llm.NewClient(llm.WithProviderSender("anthropic", neverSender{}), llm.WithCatalog(empty), llm.WithDefaultModel("claude-x"))
	c.Require().NoError(err)
	c.Eq(0, compactionContextWindow(unknown, providers.KindAnthropic, "not-in-catalog", 0), "model the catalog does not know")

	cat := routing.NewModelCatalog(nil, time.Hour, nil)
	cat.SeedForTest([]routing.CatalogEntry{{ID: "z-ai/glm-5.3-flash", Created: 1, ContextLength: 1048576}})
	known, err := llm.NewClient(llm.WithProviderSender("anthropic", neverSender{}), llm.WithCatalog(cat), llm.WithDefaultModel("claude-x"))
	c.Require().NoError(err)
	c.Eq(1048576, compactionContextWindow(known, providers.KindAnthropicOpenRouter, "z-ai/glm-5.3-flash", 0), "known model")
	c.NotEq(prefillFallbackContext, compactionContextWindow(unknown, providers.KindAnthropic, "not-in-catalog", 0),
		"unknown window fell back to the prefill window")
}

// TestCompactionContextWindowOverrideWins pins the precedence the Responses
// providers depend on: a window the serving provider reported beats the
// catalog's figure for the very same model. Without it a ChatGPT-kind model
// absent from (or misdescribed by) the catalog could only ever compact against
// a guess, and override 0 must still ask the catalog exactly as before.
func TestCompactionContextWindowOverrideWins(t *testing.T) {
	c := assert.NewCollecting(t)
	cat := routing.NewModelCatalog(nil, time.Hour, nil)
	cat.SeedForTest([]routing.CatalogEntry{{ID: "openai/gpt-6.1-sol", Created: 1, ContextLength: 400000}})
	client, err := llm.NewClient(llm.WithProviderSender("anthropic", neverSender{}), llm.WithCatalog(cat), llm.WithDefaultModel("claude-x"))
	c.Require().NoError(err)

	c.Eq(400000, compactionContextWindow(client, providers.KindOpenAIResponsesChatGPT, "gpt-6.1-sol", 0),
		"override 0 did not ask the catalog through catalogModelID")
	c.Eq(500000, compactionContextWindow(client, providers.KindOpenAIResponsesChatGPT, "gpt-6.1-sol", 500000),
		"the provider-reported window did not beat a catalog that knows the model")
}

// TestCatalogModelIDForResponsesKinds pins the mapping both catalog lookups go
// through: a Responses-kind provider serves OpenAI models under bare ids while
// the catalog indexes them with the "openai/" vendor prefix, and every other
// kind is already catalog-shaped.
func TestCatalogModelIDForResponsesKinds(t *testing.T) {
	c := assert.NewCollecting(t)

	c.Eq("openai/gpt-6.1-sol", catalogModelID(providers.KindOpenAIResponses, "gpt-6.1-sol"),
		"openai-responses bare id")
	c.Eq("openai/gpt-6.1-sol", catalogModelID(providers.KindOpenAIResponsesChatGPT, "gpt-6.1-sol"),
		"openai-responses-chatgpt bare id")
	c.Eq("openai/gpt-6.1-sol", catalogModelID(providers.KindOpenAIResponsesChatGPT, "openai/gpt-6.1-sol"),
		"an already-slashed id must not be prefixed twice")
	c.Eq("gpt-6.1-sol", catalogModelID(providers.KindOpenAI, "gpt-6.1-sol"),
		"kind openai is already catalog-shaped")
	c.Eq("z-ai/glm-5.3-flash", catalogModelID(providers.KindAnthropicOpenRouter, "z-ai/glm-5.3-flash"),
		"an OpenRouter id passes through unchanged")
}

// TestPricerMapsResponsesModelToCatalogID pins that the turn pricer resolves a
// Responses-kind model through catalogModelID: the catalog knows only
// "openai/gpt-x", so a bare "gpt-x" prices ONLY for a Responses kind. For any
// other kind the id is left alone and the lookup misses, which is the
// pre-existing behavior.
func TestPricerMapsResponsesModelToCatalogID(t *testing.T) {
	c := assert.NewCollecting(t)
	cat := routing.NewModelCatalog(nil, time.Hour, nil)
	cat.SeedForTest([]routing.CatalogEntry{{ID: "openai/gpt-x", Created: 1, Pricing: &routing.ModelPricing{
		PromptUSD:     0.000001,
		CompletionUSD: 0.000002,
	}}})
	client, err := llm.NewClient(llm.WithProviderSender("anthropic", neverSender{}), llm.WithCatalog(cat), llm.WithDefaultModel("claude-x"))
	c.Require().NoError(err)

	responses := pricerFor(client, providers.KindOpenAIResponses)
	c.Require().NotNil(responses, "pricerFor must return a pricer for a client with a catalog")
	price, ok := responses("gpt-x")
	c.True(ok, "a Responses kind did not price gpt-x from the openai/gpt-x catalog entry")
	c.Eq(0.000001, price.PromptUSD, "prompt price for gpt-x")

	anthropic := pricerFor(client, providers.KindAnthropic)
	_, ok = anthropic("gpt-x")
	c.False(ok, "kind anthropic must not map gpt-x to openai/gpt-x")
}

// TestCompactionEngineRegistersHook drives a real turn over an in-memory
// conversation seeded with enough history to sit past the window, and pins the
// wire order: compaction_start, then compaction_end, then the turn's agent_end.
// It is the end-to-end proof that NewEngine registered the observer and the
// emitter turned llm's boundary into frames the daemon/TUI consume.
func TestCompactionEngineRegistersHook(t *testing.T) {
	c := assert.NewCollecting(t)
	sink := &recordingSink{}
	sender := scriptedSender(t,
		// 1. the compaction summary call
		`{"id":"msg_s","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn",
		 "content":[{"type":"text","text":"<summary>handover</summary>"}],
		 "usage":{"input_tokens":200,"output_tokens":20}}`,
		// 2. the real call the compacted turn proceeds with
		sampleEndTurn,
	)
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, sender, func(cfg *EngineConfig) {
		cfg.ConvOpts = append(cfg.ConvOpts, llm.WithCompaction(llm.CompactionPolicy{
			ContextWindowFn: func() int { return 100_000 },
		}))
		cfg.NativeSink = sink
	})

	// Four large rows push the byte-estimated history well past the 100k window,
	// so the very first Continue's proactive check fires. Alternating roles keep
	// mergeForRequest from collapsing them.
	big := strings.Repeat("x", 100_000)
	c.Require().NoError(eng.conv.SeedHistory(context.Background(), []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(big)),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock(big)),
		anthropic.NewUserMessage(anthropic.NewTextBlock(big)),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock(big)),
	}))

	eng.HandlePrompt("go")
	eng.Wait()

	types := frameTypes(t, out.String())
	startAt, endAt, agentEndAt := indexOf(types, "compaction_start"), indexOf(types, "compaction_end"), indexOf(types, "agent_end")
	c.Require().GreaterOrEqual(0, startAt, "no compaction_start frame; got %v", types)
	c.Require().GreaterOrEqual(0, endAt, "no compaction_end frame; got %v", types)
	c.Require().GreaterOrEqual(0, agentEndAt, "no agent_end frame; got %v", types)
	c.LessOrEqual(endAt, startAt, "compaction_start must precede compaction_end; got %v", types)
	c.LessOrEqual(agentEndAt, endAt, "compaction_end must precede the turn's agent_end; got %v", types)

	// The durable boundary rode along with the successful compaction.
	var boundaries int
	for _, ev := range sink.events {
		if cb := ev.GetCompactionBoundary(); cb != nil {
			boundaries++
			c.Eq("threshold", cb.GetTrigger(), "native boundary trigger")
			c.Eq(int32(200), cb.GetPreTokens(), "native boundary pre_tokens")
		}
	}
	c.Eq(1, boundaries, "native CompactionBoundary events")
}

// indexOf returns the position of want in types, or -1.
func indexOf(types []string, want string) int {
	for i, t := range types {
		if t == want {
			return i
		}
	}
	return -1
}
