package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/fundi/internal/child"
	"git.graveland.dev/brent/rafiki/routing"
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
	if err := json.Unmarshal([]byte(costResp), &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

// A priced turn must carry a per-component cost breakdown on its usage, and the
// pricer must be queried with the SERVED model id from the response.
func TestMapAssistantMessagePricesUsage(t *testing.T) {
	var queried []string
	resp := mustCostResp(t)

	mapped := MapAssistantMessage(resp, "anthropic", stubPricer(t, "claude-sonnet-4-5-20250929", &queried))

	wantInput := 1000 * 0.000003
	wantOutput := 200 * 0.000015
	wantCacheRead := 5000 * 0.0000003
	wantCacheWrite := 400 * 0.00000375
	wantTotal := wantInput + wantOutput + wantCacheRead + wantCacheWrite

	got := mapped.Usage.Cost
	if got.Input != wantInput || got.Output != wantOutput ||
		got.CacheRead != wantCacheRead || got.CacheWrite != wantCacheWrite {
		t.Errorf("cost components = %+v, want input=%v output=%v cacheRead=%v cacheWrite=%v",
			got, wantInput, wantOutput, wantCacheRead, wantCacheWrite)
	}
	if got.Total != wantTotal {
		t.Errorf("cost total = %v, want %v", got.Total, wantTotal)
	}
	if len(queried) == 0 || queried[0] != "claude-sonnet-4-5-20250929" {
		t.Errorf("pricer queried with %v, want the served model id claude-sonnet-4-5-20250929", queried)
	}
	// Token counts must be untouched by the pricing change.
	if mapped.Usage.Input != 1000 || mapped.Usage.Output != 200 {
		t.Errorf("usage tokens = %+v, want input=1000 output=200", mapped.Usage)
	}
}

// Negative control: no pricer at all (the fake-sender / offline case) must
// leave cost zero rather than panicking.
func TestMapAssistantMessageNilPricerIsFree(t *testing.T) {
	mapped := MapAssistantMessage(mustCostResp(t), "anthropic", nil)
	if mapped.Usage.Cost != (child.PiCost{}) {
		t.Fatalf("cost = %+v, want zero with a nil pricer", mapped.Usage.Cost)
	}
	if mapped.Usage.Input != 1000 {
		t.Errorf("tokens must still be mapped with a nil pricer, got %+v", mapped.Usage)
	}
}

// Negative control: an unpriced model (pricer returns ok=false) is free.
func TestMapAssistantMessageUnpricedModelIsFree(t *testing.T) {
	mapped := MapAssistantMessage(mustCostResp(t), "anthropic", stubPricer(t, "some-other-model", nil))
	if mapped.Usage.Cost != (child.PiCost{}) {
		t.Fatalf("cost = %+v, want zero for an unpriced model", mapped.Usage.Cost)
	}
}

// agent_end reports the turn total, so cost must accumulate across the turn's
// assistant messages the same way tokens do.
func TestAgentEndSumsCostAcrossTurns(t *testing.T) {
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
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Fatalf("bad frame %q: %v", l, err)
		}
		if probe.Type == "agent_end" {
			if err := json.Unmarshal([]byte(l), &end); err != nil {
				t.Fatalf("agent_end unmarshal: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no agent_end frame emitted")
	}
	if want := single.Total * 2; end.Usage.Cost.Total != want {
		t.Errorf("agent_end cost total = %v, want 2 turns = %v", end.Usage.Cost.Total, want)
	}
	if want := single.Input * 2; end.Usage.Cost.Input != want {
		t.Errorf("agent_end cost input = %v, want %v", end.Usage.Cost.Input, want)
	}
	if end.Usage.Input != 2000 {
		t.Errorf("agent_end input tokens = %d, want 2000", end.Usage.Input)
	}
}
