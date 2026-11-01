// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// responsesProviderSet is the shape a daemon that serves a Responses provider
// has: the default Anthropic entry alongside one Responses-kind provider.
// Keeping both is the point: the mapping under test is resolved from the
// configured kinds, and it must not reach a model the Anthropic entry's kind
// would have recorded catalog-shaped.
func responsesProviderSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(`
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
`))
	assert.NewAborting(t).NoError(err, "Parse the test provider set")
	return set
}

// twoIDCatalog indexes the SAME slug under both id shapes at different prices,
// so a rollup that consults the wrong one is visible in the number rather than
// hidden behind an equal price. 100 input + 50 output tokens:
//
//	anthropic/claude-sonnet-5 -> 0.2
//	openai/gpt-6.1-sol        -> 0.2
//	openai/claude-sonnet-5    -> 2.0 (the wrong answer, 10x)
func twoIDCatalog(t *testing.T) *routing.ModelCatalog {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-sonnet-5", Created: 1, Pricing: &routing.ModelPricing{
			PromptUSD: 0.001, CompletionUSD: 0.002,
		}},
		{ID: "openai/gpt-6.1-sol", Created: 2, Pricing: &routing.ModelPricing{
			PromptUSD: 0.001, CompletionUSD: 0.002,
		}},
		{ID: "openai/claude-sonnet-5", Created: 3, Pricing: &routing.ModelPricing{
			PromptUSD: 0.010, CompletionUSD: 0.020,
		}},
	})
	return cat
}

// usageJSON is one canned assistant message reporting `model` with 100 input
// and 50 output tokens, which is the shape capture stores on the turn row.
func usageJSON(model string) string {
	return `{"id":"msg_cost","type":"message","role":"assistant","model":"` + model + `",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
}

// TestConversationCostPricesResponsesModelByCatalogID is the spend-control
// guard: a Responses-kind provider serves "gpt-6.1-sol" while the catalog
// indexes "openai/gpt-6.1-sol", and pricing the captured row by its bare id
// used to SKIP it. A skipped row is indistinguishable from a free one, so a
// child's lifetime maxCost guardrail reset every turn instead of accumulating.
func TestConversationCostPricesResponsesModelByCatalogID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		func(*msg.Request) (*msg.Response, error) {
			return cannedMessage(usageJSON("gpt-6.1-sol")), nil
		},
	}}
	c, err := NewClient(
		WithProviders(responsesProviderSet(t)),
		WithProviderSender("oa", sender),
		WithStore(pool),
		WithCatalog(twoIDCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("oa/gpt-6.1-sol"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := c.ConversationCost(ctx, conv.ID)
	want := 100*0.001 + 50*0.002 // the openai/gpt-6.1-sol entry, not 0
	ck.LessOrEqual(1e-9, math.Abs(got-want), "ConversationCost = %v, want %v (the openai/<slug> list price)", got, want)
}

// TestConversationCostUnchangedForAnthropic is the other half of the guard: the
// same fixture, the same provider set (which DOES configure a Responses kind,
// so the mapping is available), but turns served by the Anthropic entry. The
// stored id is catalog-shaped, so it must price at its own entry and never at
// the "openai/"-prefixed one.
func TestConversationCostUnchangedForAnthropic(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		func(*msg.Request) (*msg.Response, error) {
			return cannedMessage(usageJSON("claude-sonnet-5")), nil
		},
	}}
	c, err := NewClient(
		WithProviders(responsesProviderSet(t)),
		WithProviderSender("anthropic", sender),
		WithStore(pool),
		WithCatalog(twoIDCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-sonnet-5"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := c.ConversationCost(ctx, conv.ID)
	want := 100*0.001 + 50*0.002 // anthropic/claude-sonnet-5, NOT openai/claude-sonnet-5 (2.0)
	ck.LessOrEqual(1e-9, math.Abs(got-want), "ConversationCost = %v, want %v (the raw id's own price)", got, want)
}

// TestConversationCostTurnPricingTriesRawIDFirst pins the ordering and the
// candidate set directly, because the DB-backed rollup tests skip without
// RAFIKI_TEST_DSN: a raw-id hit wins over the configured kind's rewrite, a bare
// Responses id is still found through it, and a configuration with no Responses
// kind produces no second candidate at all.
func TestConversationCostTurnPricingTriesRawIDFirst(t *testing.T) {
	ck := assert.NewCollecting(t)
	cat := twoIDCatalog(t)

	c, err := NewClient(WithProviders(responsesProviderSet(t)), WithCatalog(cat), WithLogger(testLogger(t)))
	ck.Require().NoError(err)
	mp, ok := c.turnPricing("claude-sonnet-5")
	ck.Require().True(ok, "turnPricing(claude-sonnet-5)")
	ck.LessOrEqual(1e-9, math.Abs(0.001-mp.PromptUSD), "raw id priced at %v/1k, want 0.001 (the unprefixed entry, not 0.010)", mp.PromptUSD)
	mp, ok = c.turnPricing("gpt-6.1-sol")
	ck.Require().True(ok, "turnPricing(gpt-6.1-sol) must find openai/gpt-6.1-sol")
	ck.LessOrEqual(1e-9, math.Abs(0.001-mp.PromptUSD), "mapped id prompt price = %v, want 0.001", mp.PromptUSD)

	// The shipped default configures no Responses kind, so it adds no candidate:
	// a bare OpenAI id stays unpriced exactly as before this mapping existed.
	plain, err := NewClient(WithCatalog(cat), WithLogger(testLogger(t)))
	ck.Require().NoError(err)
	_, ok = plain.turnPricing("gpt-6.1-sol")
	ck.False(ok, "turnPricing(gpt-6.1-sol) without a Responses kind must miss, not widen")
}

// pinSpec is the routing spec the pin tests route under. It must be NON-zero:
// a zero spec takes the pre-spec path, which never pins. The quant floor rides
// a pinned body through Spec.PinPrefs, so a pinned request keeps its data
// policy.
func pinSpec() routing.Spec {
	return routing.Spec{Sort: routing.SortPrice, Quant: []string{"fp8+"}}
}

// pinQuantJSON is pinSpec's expanded quant floor, as it appears on the wire.
const pinQuantJSON = `["int8","fp8","mxfp8","fp16","bf16","fp32"]`

// respondWith answers every send with a copy of resp.
func respondWith(resp *msg.Response) func(*msg.Request) (*msg.Response, error) {
	return func(*msg.Request) (*msg.Response, error) {
		out := *resp
		return &out, nil
	}
}

// servedResponse is one completed OpenRouter turn the named backend served. The
// prompt is large and cache-read is a hit, so the cache rule never fires and a
// test that is not about caching cannot accidentally eject anyone.
func servedResponse(provider string) *msg.Response {
	return &msg.Response{
		ID: "msg_pin", Model: guardModel, Provider: provider, StopReason: msg.StopEndTurn,
		Usage:   msg.Usage{Input: msg.Ptr(5000), Output: msg.Ptr(5), CacheRead: msg.Ptr(4000)},
		Content: []msg.Block{msg.Text{Text: "ok"}},
	}
}

// pinTestConversation builds a CAPTURED conversation over set/sender under the
// given guard and routing spec. The store is not optional: the guard only
// remembers a turn it can attribute to a conversation, which is the capture
// path's convID, and PinFor is what a later turn pins from.
func pinTestConversation(t *testing.T, pool *pgxpool.Pool, set *providers.Set, model string,
	g *routing.ProviderGuard, spec routing.Spec, sender Sender) (*Client, *Conversation) {
	t.Helper()
	ck := assert.NewAborting(t)
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("openrouter", sender),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithProviderGuard(g),
		WithRouting(spec),
	)
	ck.NoError(err, "NewClient")
	conv, err := c.Conversation(context.Background(), NewConversation("", "test"), Model(model), SystemText("sys"))
	ck.NoError(err, "Conversation")
	return c, conv
}

// pinConversation is pinTestConversation on the one-provider OpenRouter
// registry the guard tests share.
func pinConversation(t *testing.T, pool *pgxpool.Pool, g *routing.ProviderGuard, spec routing.Spec, sender Sender) (*Client, *Conversation) {
	t.Helper()
	return pinTestConversation(t, pool, guardSet(), "openrouter/"+guardModel, g, spec, sender)
}

// sendPinTurn sends one non-streaming turn, failing the test on error.
func sendPinTurn(t *testing.T, conv *Conversation) {
	t.Helper()
	_, err := conv.Send(context.Background(), UserText("hi"))
	assert.NewAborting(t).NoError(err, "Send")
}

// pinTestGuard is a guard with a durable log the test can read back.
func pinTestGuard(t *testing.T) (*routing.ProviderGuard, *recordingSink) {
	t.Helper()
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	sink := &recordingSink{}
	g.SetSink(sink)
	return g, sink
}

// TestPinnedRequestSendsOnlyThePin is the headline behaviour: once a
// conversation has been served by a provider, its next request asks for that
// provider and nothing else. The body carries exactly {"only":[slug]} plus the
// spec's data policy — no sort, no order and no ignore, because a named only
// decides who serves and bypasses every ban by construction.
func TestPinnedRequestSendsOnlyThePin(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	sendPinTurn(t, conv) // nothing remembered yet: no pin
	sendPinTurn(t, conv) // pinned to deepinfra

	ck.Require().Eq(2, openrouter.calls, "upstream requests")
	ck.NotStrContains(wireBody(t, openrouter, 0), `"only"`,
		"the first turn of a conversation has nothing to pin to")
	body := wireBody(t, openrouter, 1)
	ck.StrContains(body, `"provider":{"only":["deepinfra"],"quantizations":`+pinQuantJSON+`}`,
		"the second request must be pinned to the served provider, wire body =")
	ck.NotStrContains(body, `"sort"`, "a pinned request sends no sort")
	ck.NotStrContains(body, `"ignore"`, "a pinned request sends no ignore")
	ck.NotStrContains(body, `"order"`, "a pinned request sends no order")
}

// TestPinSkippedForOperatorOnlyAndExclude pins operator authority: an automatic
// pin is only ever added when the operator left the choice open. A spec only, a
// spec exclude, or a model declaration's own only all suppress it.
func TestPinSkippedForOperatorOnlyAndExclude(t *testing.T) {
	pool := convTestPool(t)

	t.Run("spec only", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, _ := pinTestGuard(t)
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondWith(servedResponse("DeepInfra")),
		}}
		_, conv := pinConversation(t, pool, g, routing.Spec{Sort: routing.SortPrice, Only: []string{"fireworks"}}, openrouter)
		sendPinTurn(t, conv)
		sendPinTurn(t, conv)

		body := wireBody(t, openrouter, 1)
		ck.StrContains(body, `"provider":{"only":["fireworks"]`, "the spec only must win, wire body =")
		ck.NotStrContains(body, "deepinfra", "a spec only must suppress the automatic pin")
	})

	t.Run("spec exclude", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, _ := pinTestGuard(t)
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondWith(servedResponse("DeepInfra")),
		}}
		_, conv := pinConversation(t, pool, g, routing.Spec{Sort: routing.SortPrice, Exclude: []string{"deepinfra"}}, openrouter)
		sendPinTurn(t, conv)
		sendPinTurn(t, conv)

		body := wireBody(t, openrouter, 1)
		ck.NotStrContains(body, `"only"`, "an excluded provider must not be auto-pinned, wire body =")
		ck.StrContains(body, `"ignore":["deepinfra"]`, "the spec's exclusion still rides as a ban, wire body =")
	})

	t.Run("declaration only", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, _ := pinTestGuard(t)
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondWith(servedResponse("DeepInfra")),
		}}
		_, conv := pinTestConversation(t, pool, declPinSet(t), "openrouter/glm-flash@together",
			g, routing.Spec{Sort: routing.SortPrice}, openrouter)
		sendPinTurn(t, conv)
		sendPinTurn(t, conv)

		body := wireBody(t, openrouter, 1)
		ck.StrContains(body, `"provider":{"only":["together"]`, "the declaration's only must win, wire body =")
		ck.NotStrContains(body, "deepinfra", "a declaration only must suppress the automatic pin")
	})
}

// TestPinSkippedForZeroSpec is the byte-identical guarantee: a client with no
// routing spec takes the pre-spec path, so the second turn's request is
// identical to the first's everywhere but the message history it appended —
// even though the guard has remembered where the first was served. The PinFor
// assertion proves the pin really was available and refused, not merely absent.
func TestPinSkippedForZeroSpec(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
	}}
	_, conv := pinConversation(t, pool, g, routing.Spec{}, openrouter)
	sendPinTurn(t, conv)
	sendPinTurn(t, conv)

	ck.Require().Eq(2, openrouter.calls, "upstream requests")
	ck.Eq(bodyWithoutMessages(t, wireBody(t, openrouter, 0)), bodyWithoutMessages(t, wireBody(t, openrouter, 1)),
		"a zero spec must produce identical bodies apart from the appended history; the pre-spec path never pins")
	ck.NotStrContains(wireBody(t, openrouter, 1), `"provider"`,
		"a zero spec with nothing ejected sends no provider object at all")
	slug, ok := g.PinFor(time.Now(), conv.ID, guardModel)
	ck.True(ok, "the guard must have remembered the served provider, so this test would catch a pin")
	ck.Eq("deepinfra", slug, "remembered provider")
}

// bodyWithoutMessages is a wire body with its messages array removed, as a
// canonical (decode/re-encode) string — the shape two turns of one conversation
// share even though the later one carries the earlier turn's history. It is the
// part of the body routing decisions live in: model, system, tools, sampling
// knobs and the provider object.
func bodyWithoutMessages(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	delete(m, "messages")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode wire body: %v", err)
	}
	return string(out)
}

// TestPinSkippedWithoutConversationID: a send with no conversation id has no
// conversation to pin, so it never pins — even on a client that does pin the
// same model inside a conversation.
func TestPinSkippedWithoutConversationID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
	}}
	c, conv := pinConversation(t, pool, g, pinSpec(), openrouter)

	// Control: with the conversation id, the second turn pins.
	sendPinTurn(t, conv)
	sendPinTurn(t, conv)
	ck.StrContains(wireBody(t, openrouter, 1), `"only":["deepinfra"]`,
		"control: a send carrying the conversation id must pin")

	// The same client and model, with no conversation id at all.
	req := userTextRequest("openrouter/" + guardModel)
	_, err := c.send(context.Background(), SendMeta{}, req, nil)
	ck.Require().NoError(err, "send without a conversation id")
	ck.NotStrContains(wireBody(t, openrouter, 2), `"only"`,
		"a send with no conversation id must not be pinned")
}

// TestPinnedTurnRecordsPinnedProvider: the turn row says which provider the
// request forced, so a stored turn can be read back without re-deriving the
// guard's state. NULL for an unpinned turn.
func TestPinnedTurnRecordsPinnedProvider(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	sendPinTurn(t, conv)
	sendPinTurn(t, conv)

	rows, err := pool.Query(context.Background(),
		`SELECT pinned_provider FROM conversations.conversation_turn
		 WHERE conversation_id=$1::uuid ORDER BY created_at, id`, conv.ID)
	ck.Require().NoError(err, "read turn rows")
	defer rows.Close()
	var pins []*string
	for rows.Next() {
		var p *string
		ck.Require().NoError(rows.Scan(&p), "scan pinned_provider")
		pins = append(pins, p)
	}
	ck.Require().NoError(rows.Err(), "iterate turn rows")
	ck.Require().Len(pins, 2, "turn rows")
	ck.Nil(pins[0], "the first turn of a conversation is not pinned")
	ck.Require().NotNil(pins[1], "the second turn must record its pin")
	ck.Eq("deepinfra", *pins[1], "pinned_provider")
}

// TestFailureObservationIsPinned: a pinned provider that keeps failing is
// banned, and the ban is written to the durable log as an unavailable record.
// Four is the threshold the guard documents (the agent loop's own backoff has
// had its chance by then).
func TestFailureObservationIsPinned(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, sink := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
		respondErr(transientErr()),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	sendPinTurn(t, conv) // remembers deepinfra

	for i := range 4 {
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "attempt %d must surface the upstream failure", i+1)
	}

	slug, ok := g.PinFor(time.Now(), conv.ID, guardModel)
	ck.False(ok, "four failed pinned attempts must ban the provider, got %q", slug)
	rec, found := sink.find(routing.ReasonUnavailable)
	ck.Require().True(found, "the ban must reach the durable log, got %v", sink.reasons())
	ck.Eq("deepinfra", rec.Provider, "banned provider")
	ck.Eq("z-ai/glm-5.3-flash", rec.ModelLine, "banned model line")
}

// TestCanceledAttemptIsNotObserved: a cancelled turn is not evidence about the
// provider. Four cancellations must leave the pin intact — otherwise a user
// pressing ctrl-C would ban a healthy provider.
func TestCanceledAttemptIsNotObserved(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, sink := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
		respondErr(context.Canceled),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	sendPinTurn(t, conv) // remembers deepinfra

	for i := range 4 {
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().ErrorIs(err, context.Canceled, "attempt %d must surface the cancellation", i+1)
	}

	slug, ok := g.PinFor(time.Now(), conv.ID, guardModel)
	ck.True(ok, "four cancelled attempts must not ban the provider")
	ck.Eq("deepinfra", slug, "the pin must survive cancellation")
	ck.Empty(sink.reasons(), "a cancelled attempt must write no ejection")
}

// TestOutputCapObservedFromResponse: a provider that stops at max_tokens having
// produced no more than half the room it was given truncated the answer, which
// no retry against the same provider fixes — so one such turn bans it, and the
// numbers come from the RESPONSE (output) and the REQUEST (max_tokens).
func TestOutputCapObservedFromResponse(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	truncated := &msg.Response{
		ID: "msg_cap", Model: guardModel, Provider: "DeepInfra", StopReason: msg.StopMaxTokens,
		Usage:   msg.Usage{Input: msg.Ptr(100), Output: msg.Ptr(2048)},
		Content: []msg.Block{msg.Text{Text: "half an answer"}},
	}
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(truncated),
	}}
	c, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	req := userTextRequest("openrouter/" + guardModel)
	req.MaxTokens = 16384
	_, err := c.send(context.Background(), SendMeta{ConversationID: conv.ID}, req, nil)
	ck.Require().NoError(err, "send")

	ejected := g.Ejected(time.Now())
	ck.Require().Len(ejected, 1, "one ejection, got %+v", ejected)
	ck.Eq(routing.ReasonOutputCap, ejected[0].Reason, "ejection reason")
	ck.Eq("deepinfra", ejected[0].Provider, "ejected provider")
}

// TestNoEligibleResendLiftsAutomaticExclusions: a line whose providers the
// guard ejected still has somewhere to go, so OpenRouter's no-eligible
// rejection lifts the guard's own ejections and resends once. The operator's
// bans are untouched — that is LiftLine's contract.
func TestNoEligibleResendLiftsAutomaticExclusions(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, sink := pinTestGuard(t)
	// Seed an automatic ejection on the line: the guard's own verdict on
	// "together", exactly as a pinned no-eligible failure would write it.
	g.Observe(time.Now(), routing.Observation{
		Model: guardModel, Conversation: "seed", Pinned: "together",
		Failed: true, FailureKind: routing.FailureNoEligible,
	})
	ck.Require().True(len(g.IgnoredFor(time.Now(), guardModel)) == 1,
		"guard setup failed: the line must have one automatic ejection")

	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
		respondWith(servedResponse("DeepInfra")),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	_, err := conv.Send(context.Background(), UserText("hi"))
	ck.Require().NoError(err, "the resend must succeed")
	ck.Eq(2, openrouter.calls, "exactly two upstream requests: the refusal and its resend")
	ck.StrContains(wireBody(t, openrouter, 0), `"ignore":["together"]`,
		"the first request must carry the guard's ejection, wire body =")
	ck.NotStrContains(wireBody(t, openrouter, 1), `"ignore"`,
		"the resend must route around the lifted ejection, wire body =")
	rec, found := sink.find(routing.ReasonLift)
	ck.Require().True(found, "LiftLine must write a lift record, got %v", sink.reasons())
	ck.Eq("together", rec.Provider, "lifted provider")
	ck.Eq("line exhausted", rec.Note, "lift note")
}

// TestNoEligibleResendAfterPinnedRefusal: when the PIN is what OpenRouter could
// not serve, the failure observation bans it outright, so the single resend goes
// out unpinned rather than asking the same provider again.
func TestNoEligibleResendAfterPinnedRefusal(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")),
		respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
		respondWith(servedResponse("Together")),
	}}
	_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
	sendPinTurn(t, conv) // remembers deepinfra

	_, err := conv.Send(context.Background(), UserText("hi"))
	ck.Require().NoError(err, "the resend must succeed")

	ck.Require().Eq(3, openrouter.calls, "one turn, one pinned refusal, one resend")
	ck.StrContains(wireBody(t, openrouter, 1), `"only":["deepinfra"]`,
		"the refused attempt must be the pinned one, wire body =")
	ck.NotStrContains(wireBody(t, openrouter, 2), `"only"`,
		"the resend must not re-pin the provider that just refused, wire body =")
	ck.StrContains(wireBody(t, openrouter, 2), `"ignore":["deepinfra"]`,
		"the banned pin rides the resend as a ban, wire body =")
}

// TestNoEligibleOperatorOnlyDoesNotResend: when nothing but the operator's own
// decision excluded every provider, resending the same body would fail
// identically — so the error is returned after ONE request.
func TestNoEligibleOperatorOnlyDoesNotResend(t *testing.T) {
	pool := convTestPool(t)

	t.Run("operator ban", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, _ := pinTestGuard(t)
		_, err := g.Ban(context.Background(), time.Now(), "together", 0, "test ban")
		ck.Require().NoError(err, "Ban")
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
		}}
		_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
		_, err = conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "the no-eligible rejection must surface")
		ck.Eq(1, openrouter.calls, "an operator ban must not be lifted by a resend")
	})

	t.Run("spec only with an automatic ban on the line", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, sink := pinTestGuard(t)
		// A real automatic ban on this line, so hadAutomatic would be true if a
		// spec only= did not suppress it: Spec.Prefs drops the whole ignore list
		// when only= NAMES a provider, so the bans never rode in this request.
		g.Observe(time.Now(), routing.Observation{
			Model: guardModel, Conversation: "seed", Pinned: "deepinfra",
			Failed: true, FailureKind: routing.FailureNoEligible,
		})
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
		}}
		spec := pinSpec()
		spec.Only = []string{"together"}
		_, conv := pinConversation(t, pool, g, spec, openrouter)
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "the no-eligible rejection must surface")
		ck.Eq(1, openrouter.calls, "an ignore list this request never carried must not be lifted")
		ck.EqDeep([]routing.EjectReason{routing.ReasonUnavailable}, sink.reasons(),
			"the line's automatic bans must survive: only the seed ban was written")
	})

	t.Run("spec exclude", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		g, _ := pinTestGuard(t)
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
		}}
		_, conv := pinConversation(t, pool, g, routing.Spec{Sort: routing.SortPrice, Exclude: []string{"deepinfra"}}, openrouter)
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "the no-eligible rejection must surface")
		ck.Eq(1, openrouter.calls, "a spec exclude must not be lifted by a resend")
	})
}

// TestNoEligibleResendsOnlyOnce: the resend budget is one per logical send. A
// line that is still exhausted after the lift returns the error rather than
// looping.
func TestNoEligibleResendsOnlyOnce(t *testing.T) {
	noEligible := respondErr(noEligibleErr(http.StatusBadRequest,
		"No allowed providers are available for the selected model."))

	t.Run("automatic ban", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		pool := convTestPool(t)
		g, _ := pinTestGuard(t)
		g.Observe(time.Now(), routing.Observation{
			Model: guardModel, Conversation: "seed", Pinned: "together",
			Failed: true, FailureKind: routing.FailureNoEligible,
		})
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){noEligible}}
		_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "the second no-eligible must reach the caller")
		ck.Eq(2, openrouter.calls, "one resend, never a loop")
	})

	// The pinned shape is the one that could loop: the refused pin becomes an
	// automatic ejection, so a second resend decision would find something to
	// lift every time.
	t.Run("pinned refusal", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		pool := convTestPool(t)
		g, _ := pinTestGuard(t)
		openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
			respondWith(servedResponse("DeepInfra")),
			noEligible,
		}}
		_, conv := pinConversation(t, pool, g, pinSpec(), openrouter)
		sendPinTurn(t, conv) // remembers deepinfra
		_, err := conv.Send(context.Background(), UserText("hi"))
		ck.Require().Error(err, "the second no-eligible must reach the caller")
		ck.Eq(3, openrouter.calls, "one turn, the pinned refusal and ONE resend")
	})
}

// TestFallbackFailureIsNotAttributedToThePin pins the attribution rule on the
// failover path: only the PRIMARY's own attempt is evidence about the pin. A
// fallback is never pinned (its body carries no auto-pin), so its failure must
// not be judged against the provider the primary was pinned to — otherwise one
// fallback no-eligible would ban a healthy provider for four hours.
func TestFallbackFailureIsNotAttributedToThePin(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	g, _ := pinTestGuard(t)

	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondWith(servedResponse("DeepInfra")), // turn 1 is served, so turn 2 is pinned
		respondErr(overloadedErr()),
	}}
	backup := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model.")),
	}}
	set := &providers.Set{DefaultProvider: "openrouter", Providers: map[string]providers.Provider{
		"openrouter": {Name: "openrouter", Kind: providers.KindAnthropicOpenRouter, Fallback: []string{"backup"}},
		"backup":     {Name: "backup", Kind: providers.KindAnthropicOpenRouter},
	}}
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("openrouter", primary),
		WithProviderSender("backup", backup),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithProviderGuard(g),
		WithRouting(pinSpec()),
		WithBreaker(15*time.Minute),
	)
	ck.Require().NoError(err, "NewClient")
	conv, err := c.Conversation(context.Background(), NewConversation("", "test"),
		Model("openrouter/"+guardModel), SystemText("sys"))
	ck.Require().NoError(err, "Conversation")

	sendPinTurn(t, conv) // served by DeepInfra: the conversation is pinned from here
	_, err = conv.Send(context.Background(), UserText("hi"))
	ck.Require().Error(err, "both upstreams failed")
	ck.Eq(1, backup.calls, "the fallback was tried")

	ck.Empty(g.IgnoredFor(time.Now(), guardModel),
		"a fallback's no-eligible must not ban the pin: the primary's own attempt was merely overloaded")
	if slug, ok := g.PinFor(time.Now(), conv.ID, guardModel); !ok || slug != "deepinfra" {
		t.Errorf("PinFor = (%q, %v), want (deepinfra, true): one transient is not a ban", slug, ok)
	}
}
