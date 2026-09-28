// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routingTOML is a minimal openrouter-only registry with NO provider pins and
// no aliases: every provider-routing decision in the body must then come from
// the client's routing spec (or the guard), never from a pin.
const routingTOML = `
default_provider = "openrouter"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.openrouter.models.glmflash]
id = "z-ai/glm-5.3-flash"

[providers.openrouter.models.kimi]
id = "moonshotai/kimi-k3"
`

func routingSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(routingTOML))
	assert.NewAborting(t).NoError(err, "Parse routing providers.toml")
	return set
}

// newSpecClient builds a client on set with the given routing spec, optional
// guard, and openrouter sender.
func newSpecClient(t *testing.T, set *providers.Set, spec routing.Spec, guard *routing.ProviderGuard, openrouter Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithRouting(spec),
	)
	assert.NewAborting(t).NoError(err, "NewClient")
	if guard != nil {
		c.SetProviderGuard(guard)
	}
	return c
}

// ejectTogether fabricates the guard evidence that ejects "Together" for the
// glm-5.3-flash line, and fails the test if it did not take effect.
func ejectTogether(t *testing.T) *routing.ProviderGuard {
	t.Helper()
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{Provider: "Together", Model: "z-ai/glm-5.3-flash",
			Conversation: "c1", PrefixHash: "h1", InputTokens: 50000})
	}
	if ignore := g.IgnoredFor(now, "z-ai/glm-5.3-flash"); len(ignore) != 1 || ignore[0] != "together" {
		t.Fatalf("guard setup failed: IgnoredFor = %v, want [together]", ignore)
	}
	return g
}

// noEligibleErr fabricates the SDK error OpenRouter returns when its provider
// routing excludes every host for the model (the 404 "No endpoints found ..."
// family and the 400 "No allowed providers ..." one).
func noEligibleErr(status int, msg string) *anthropic.Error {
	e := &anthropic.Error{StatusCode: status}
	req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	e.Request = req
	e.Response = &http.Response{StatusCode: status}
	_ = e.UnmarshalJSON([]byte(`{"error":{"message":"` + msg + `","code":` + strconv.Itoa(status) + `}}`))
	return e
}

// TestApplyProviderPrefsZeroSpecUnchanged pins the pre-spec behaviour: with a
// zero spec, applyProviderPrefs produces byte-identical bodies to the
// pre-routing-spec client for every shape that used to reach it — no pin, a
// static pin, an alias pin, a guard ejection, extras alone, extras with a
// pin. The goldens below were harvested from the pre-spec code.
func TestApplyProviderPrefsZeroSpecUnchanged(t *testing.T) {
	ck := assert.NewCollecting(t)
	g := ejectTogether(t)
	aliasOnly := &providers.ModelAlias{ID: "z-ai/glm-5.3-flash", Only: []string{"together"}}
	noAlias := (*providers.ModelAlias)(nil)

	cases := []struct {
		name   string
		model  string
		alias  *providers.ModelAlias
		guard  *routing.ProviderGuard
		extras map[string]any
		golden string
	}{
		{"plain", "z-ai/glm-5.3-flash", noAlias, nil, nil,
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.3-flash"}`},
		{"static pin", "z-ai/glm-5.2", noAlias, nil, nil,
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.2","provider":{"only":["fireworks"]}}`},
		{"alias pin", "z-ai/glm-5.3-flash", aliasOnly, nil, nil,
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.3-flash","provider":{"only":["together"]}}`},
		{"guard ignore", "z-ai/glm-5.3-flash", noAlias, g, nil,
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.3-flash","provider":{"ignore":["together"]}}`},
		{"extras only", "z-ai/glm-5.3-flash", noAlias, nil, map[string]any{"top_k": float64(7)},
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.3-flash","top_k":7}`},
		{"extras + pin", "z-ai/glm-5.2", noAlias, nil, map[string]any{"top_k": float64(7)},
			`{"max_tokens":16,"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"model":"z-ai/glm-5.2","top_k":7,"provider":{"only":["fireworks"]}}`},
	}
	for _, tc := range cases {
		params := anthropic.MessageNewParams{Model: anthropic.Model(tc.model), MaxTokens: 16,
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
		applyProviderPrefs(&params, tc.alias, tc.guard, tc.extras, routing.Spec{})
		b, err := json.Marshal(params)
		ck.Require().NoError(err, "marshal %s", tc.name)
		ck.Eq(tc.golden, string(b), "%s: body must be byte-identical to the pre-spec client", tc.name)
	}
	ck.Require()
}

// TestRoutingSpecOnBody proves a non-zero spec lands on the wire: sort, an
// expanded quantization floor and nodata ride the provider object of a model
// with no pin, and nothing else (no only, no ignore, no zdr) leaks in.
func TestRoutingSpecOnBody(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	spec := routing.Spec{Sort: routing.SortPrice, Quant: []string{"fp8+"}, NoData: true}
	c := newSpecClient(t, routingSet(t), spec, nil, openrouter)
	sendAliasParams(t, c, "openrouter/kimi")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body,
		`"provider":{"sort":"price","quantizations":["int8","fp8","mxfp8","fp16","bf16","fp32"],"data_collection":"deny"}`,
		"sort + expanded quant floor + nodata must ride the provider object, wire body =")
	ck.NotStrContains(body, `"only"`, "no pin, so the spec must not invent one")
	ck.NotStrContains(body, `"ignore"`, "no guard, so no ignore")
	ck.NotStrContains(body, `"zdr"`, "zdr unset")
}

// TestRoutingOnlyDropsGuardIgnores proves a spec only is an explicit routing
// decision that BYPASSES the guard: the guard has ejected "together" for the
// model line, the spec onlys "fireworks", and the body carries only
// fireworks with NO ignore list.
func TestRoutingOnlyDropsGuardIgnores(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	spec := routing.Spec{Only: []string{"fireworks"}, Sort: routing.SortPrice}
	c := newSpecClient(t, routingSet(t), spec, ejectTogether(t), openrouter)
	sendAliasParams(t, c, "openrouter/glmflash")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"sort":"price"}`,
		"spec only must replace the pin slot and drop the guard's ejection, wire body =")
	ck.NotStrContains(body, `"together"`, "the guard's ejection leaked past a spec only")
	ck.NotStrContains(body, `"ignore"`, "ignore must be absent entirely under a spec only")
}

// TestRoutingOnlyKeepsZDR proves the data-policy boundary survives an only:
// ZDR rides along beside the spec's only (an only never bypasses data policy,
// per routing.Spec.Prefs).
func TestRoutingOnlyKeepsZDR(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	spec := routing.Spec{Only: []string{"fireworks"}, ZDR: true}
	c := newSpecClient(t, routingSet(t), spec, nil, openrouter)
	sendAliasParams(t, c, "openrouter/glmflash")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"zdr":true}`,
		"zdr must ride along beside the spec only, wire body =")
}

// TestRoutingAliasOnlyLosesToSpecOnly proves precedence: the alias's only pin
// (glm-flash@together -> together) loses to the spec's own only — the spec is
// the more explicit, more recent declaration of where the request may go.
func TestRoutingAliasOnlyLosesToSpecOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	spec := routing.Spec{Only: []string{"fireworks"}}
	c := newSpecClient(t, aliasPinSet(t), spec, nil, openrouter)
	sendAliasParams(t, c, "openrouter/glm-flash@together")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"]}`,
		"the spec only must win the only slot, wire body =")
	ck.NotStrContains(body, `"together"`, "the alias pin leaked past the spec only")
}

// TestRoutingNoEligibleErrorNamesSpec proves the no-eligible rejection is
// decorated with the spec that caused it: OpenRouter's 404 no-endpoints error
// comes back as "no provider can serve <model> under routing [<spec>]: ..."
// with the SDK error still reachable (errors.As), and stays unclassified —
// not retryable (deterministic: the same body fails forever) and not
// failover-worthy. A zero spec does not decorate.
func TestRoutingNoEligibleErrorNamesSpec(t *testing.T) {
	ck := assert.NewCollecting(t)
	spec := routing.Spec{Sort: routing.SortPrice}
	c := newSpecClient(t, routingSet(t), spec, nil,
		&scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondErr(noEligibleErr(http.StatusNotFound, "No endpoints found matching your data policy")),
		}})
	params := anthropic.MessageNewParams{Model: anthropic.Model("openrouter/glmflash"), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	_, err := c.SendParams(context.Background(), SendMeta{}, params)
	ck.Require().Error(err, "the no-endpoints rejection must surface")

	want := "no provider can serve z-ai/glm-5.3-flash under routing [sort=price]: "
	ck.StrContains(err.Error(), want, "the error must name the model and the spec, got: %s", err)
	var apiErr *anthropic.Error
	ck.True(errors.As(err, &apiErr), "the SDK error must stay reachable through the wrap: %v", err)
	ck.Eq(http.StatusNotFound, apiErr.StatusCode, "the wrapped status is preserved")
	ck.False(routing.Retryable(err), "deterministic: the same request fails forever, never retry")
	ck.False(routing.FailoverWorthy(err), "not failover-worthy either (a 404 is not a health signal)")

	// Zero-spec control: the identical upstream failure passes through
	// undecorated — the wrap names a spec only when there is one.
	c0 := newSpecClient(t, routingSet(t), routing.Spec{}, nil,
		&scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondErr(noEligibleErr(http.StatusNotFound, "No endpoints found matching your data policy")),
		}})
	_, err0 := c0.SendParams(context.Background(), SendMeta{}, params)
	ck.Require().Error(err0, "the raw rejection must still surface")
	ck.NotStrContains(err0.Error(), "under routing", "zero spec must not decorate, got: %s", err0)
}

// TestRoutingNoEligibleClassification pins noEligibleUpstream narrowly: the
// OpenRouter no-eligible family is matched by status (400/404) AND message
// fragment — a 5xx, a 429 or any other 4xx body is not the class, even when
// quoting the same words.
func TestRoutingNoEligibleClassification(t *testing.T) {
	ck := assert.NewCollecting(t)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 data policy", noEligibleErr(http.StatusNotFound, "No endpoints found matching your data policy"), true},
		{"404 no endpoints", noEligibleErr(http.StatusNotFound, "No endpoints found for z-ai/glm-5.2"), true},
		{"400 no allowed providers", noEligibleErr(http.StatusBadRequest, "No allowed providers are available for the selected model."), true},
		{"404 other message", noEligibleErr(http.StatusNotFound, "No such model: z-ai/glm-5.2"), false},
		{"500 quoting the words", noEligibleErr(http.StatusInternalServerError, "No endpoints found matching your data policy"), false},
		{"429 quoting the words", noEligibleErr(http.StatusTooManyRequests, "No allowed providers are available."), false},
		{"plain 400", promptTooLargeErr(), false},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		ck.Eq(tc.want, noEligibleUpstream(tc.err), "%s", tc.name)
	}
	ck.Require()
}
