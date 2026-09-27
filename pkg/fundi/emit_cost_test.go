package fundi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// stubPricer prices exactly one model, with a distinct rate per component so a
// cross-wired formula yields a different number. Recording the queried model
// lets a test assert WHICH id was priced — the served id from the response, not
// a requested alias.
func stubPricer(t *testing.T, wantModel string, queried *[]string) Pricer {
	t.Helper()
	return func(model string) (routing.ModelPricing, bool) {
		if queried != nil {
			*queried = append(*queried, model)
		}
		if model != wantModel {
			return routing.ModelPricing{}, false
		}
		return routing.ModelPricing{
			PromptUSD:     0.000003,
			CompletionUSD: 0.000015,
			CacheReadUSD:  0.0000003,
			CacheWriteUSD: 0.00000375,
		}, true
	}
}

const costResp = `{
 "id":"msg_c","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929",
 "stop_reason":"end_turn",
 "content":[{"type":"text","text":"done"}],
 "usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":5000,"cache_creation_input_tokens":400}}`

func mustCostResp(t *testing.T) *anthropic.Message {
	t.Helper()
	var resp anthropic.Message
	assert.NewAborting(t).NoError(json.Unmarshal([]byte(costResp), &resp))
	return &resp
}

// A priced turn must carry a per-component cost breakdown on its usage, and the
// pricer must be queried with the SERVED model id from the response.
func TestMapAssistantMessagePricesUsage(t *testing.T) {
	c := assert.NewCollecting(t)
	var queried []string
	resp := mustCostResp(t)

	mapped := MapAssistantMessage(resp, "anthropic", stubPricer(t, "claude-sonnet-4-5-20250929", &queried))

	wantInput := 1000 * 0.000003
	wantOutput := 200 * 0.000015
	wantCacheRead := 5000 * 0.0000003
	wantCacheWrite := 400 * 0.00000375
	wantTotal := wantInput + wantOutput + wantCacheRead + wantCacheWrite

	got := mapped.Usage.Cost
	c.False(got.Input != wantInput || got.Output != wantOutput ||
		got.CacheRead != wantCacheRead || got.CacheWrite != wantCacheWrite, "cost components = %+v, want input=%v output=%v cacheRead=%v cacheWrite=%v", got, wantInput, wantOutput, wantCacheRead, wantCacheWrite)
	c.Eq(wantTotal, got.Total, "cost total")
	c.False(len(queried) == 0 || queried[0] != "claude-sonnet-4-5-20250929", "pricer queried with %v, want the served model id claude-sonnet-4-5-20250929", queried)
	// Token counts must be untouched by the pricing change.
	c.False(mapped.Usage.Input != 1000 || mapped.Usage.Output != 200, "usage tokens = %+v, want input=1000 output=200", mapped.Usage)
}

// Negative control: no pricer at all (the fake-sender / offline case) must
// leave cost zero rather than panicking.
func TestMapAssistantMessageNilPricerIsFree(t *testing.T) {
	c := assert.NewCollecting(t)
	mapped := MapAssistantMessage(mustCostResp(t), "anthropic", nil)
	c.Require().Eq((child.PiCost{}), mapped.Usage.Cost, "cost")
	c.Eq(1000, mapped.Usage.Input, "tokens must still be mapped with a nil pricer, got %+v", mapped.Usage)
}

// Negative control: an unpriced model (pricer returns ok=false) is free.
func TestMapAssistantMessageUnpricedModelIsFree(t *testing.T) {
	mapped := MapAssistantMessage(mustCostResp(t), "anthropic", stubPricer(t, "some-other-model", nil))
	assert.NewAborting(t).Eq((child.PiCost{}), mapped.Usage.Cost, "cost")
}

// agent_end reports the turn total, so cost must accumulate across the turn's
// assistant messages the same way tokens do.
func TestAgentEndSumsCostAcrossTurns(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := mustCostResp(t)
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic", stubPricer(t, "claude-sonnet-4-5-20250929", nil))

	em.AgentStart()
	em.AssistantTurn(resp)
	em.AssistantTurn(resp)
	em.AgentEnd()

	single := MapAssistantMessage(resp, "anthropic", stubPricer(t, "claude-sonnet-4-5-20250929", nil)).Usage.Cost

	var end struct {
		Type  string `json:"type"`
		Usage struct {
			Cost struct {
				Input, Output, CacheRead, CacheWrite, Total float64
			} `json:"cost"`
			Input int `json:"input"`
		} `json:"usage"`
	}
	var found bool
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var probe struct {
			Type string `json:"type"`
		}
		c.Require().NoError(json.Unmarshal([]byte(l), &probe), "bad frame %q", l)
		if probe.Type == "agent_end" {
			c.Require().NoError(json.Unmarshal([]byte(l), &end), "agent_end unmarshal")
			found = true
		}
	}
	c.Require().True(found, "no agent_end frame emitted")
	c.Eq(single.Total*2, end.Usage.Cost.Total, "agent_end cost total")
	c.Eq(single.Input*2, end.Usage.Cost.Input, "agent_end cost input")
	c.Eq(2000, end.Usage.Input, "agent_end input tokens")
}

// agentEndCostAndCount extracts the agent_end frame's usage.cost.total and
// len(messages) from a stream of emitted ndjson frames, used to compare the
// streaming and non-streaming emission paths below.
func agentEndCostAndCount(t *testing.T, out string) (total float64, messages int) {
	t.Helper()
	c := assert.NewAborting(t)
	var end struct {
		Usage struct {
			Cost struct{ Total float64 } `json:"cost"`
		} `json:"usage"`
		Messages []json.RawMessage `json:"messages"`
	}
	var found bool
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var probe struct{ Type string }
		c.NoError(json.Unmarshal([]byte(l), &probe), "bad frame %q", l)
		if probe.Type == "agent_end" {
			c.NoError(json.Unmarshal([]byte(l), &end), "agent_end unmarshal")
			found = true
		}
	}
	c.True(found, "no agent_end frame emitted")
	return end.Usage.Cost.Total, len(end.Messages)
}

// TestStreamEndFoldsCostIdenticallyToAssistantTurn locks down the invariant
// that StreamEnd's bookkeeping (accumulate + addUsage) matches AssistantTurn's
// exactly: a turn driven through StreamStart/StreamEnd must report the same
// agent_end cost total and the same accumulated message count as the
// equivalent turn driven through the non-streaming AssistantTurn. If these two
// paths diverge, per-turn cost silently differs depending on whether streaming
// was used.
func TestStreamEndFoldsCostIdenticallyToAssistantTurn(t *testing.T) {
	c := assert.NewAborting(t)
	resp := mustCostResp(t)
	pricer := stubPricer(t, "claude-sonnet-4-5-20250929", nil)

	var outA bytes.Buffer
	feA := NewFrontend(strings.NewReader(""), &outA, &fakeHandler{})
	emA := NewEmitter(feA, "anthropic", pricer)
	emA.AgentStart()
	emA.AssistantTurn(resp)
	emA.AgentEnd()

	var outB bytes.Buffer
	feB := NewFrontend(strings.NewReader(""), &outB, &fakeHandler{})
	emB := NewEmitter(feB, "anthropic", pricer)
	msg := MapAssistantMessage(resp, "anthropic", pricer)
	emB.AgentStart()
	emB.StreamStart(msg)
	emB.StreamEnd(msg)
	emB.AgentEnd()

	totalA, messagesA := agentEndCostAndCount(t, outA.String())
	totalB, messagesB := agentEndCostAndCount(t, outB.String())

	c.Eq(totalB, totalA, "cost total diverges between paths: AssistantTurn")
	c.NotEq(0, totalA, "test is vacuous: expected a nonzero priced cost total")
	c.Eq(messagesB, messagesA, "accumulated message count diverges between paths: AssistantTurn")
}
