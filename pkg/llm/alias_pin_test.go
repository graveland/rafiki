// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// aliasPinSet builds the two-aliases-one-id registry through the REAL config
// path (providers.Parse), so the test also proves the toml `only` key
// round-trips: an alias pin is declared per alias, never per model id.
const aliasPinTOML = `
default_provider = "openrouter"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.openrouter]
kind = "anthropic-openrouter"
api_key_env = "OPENROUTER_API_KEY"

[providers.openrouter.models."glm-flash@together"]
id   = "z-ai/glm-5.3-flash"
only = ["together"]

[providers.openrouter.models."glm-flash@fireworks"]
id   = "z-ai/glm-5.3-flash"
only = ["fireworks"]

[providers.openrouter.models.glm52-novita]
id   = "z-ai/glm-5.2"
only = ["novita"]
`

func aliasPinSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(aliasPinTOML))
	assert.NewAborting(t).NoError(err, "Parse alias-pin providers.toml")
	return set
}

// sendAliasParams sends one non-streaming turn for model and returns nothing;
// failures fail the test.
func sendAliasParams(t *testing.T, c *Client, model string) {
	t.Helper()
	params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	_, err := c.SendParams(context.Background(), SendMeta{}, params)
	assert.NewAborting(t).NoError(err, "SendParams(%s)", model)
}

// wireBody marshals one captured request the way the wire sees it, including
// the SDK's ExtraFields ("provider").
func wireBody(t *testing.T, reqs []anthropic.MessageNewParams, i int) string {
	t.Helper()
	b, err := json.Marshal(reqs[i])
	assert.NewAborting(t).NoError(err, "marshal wire params")
	return string(b)
}

// TestSendParamsAliasOnlyPinsProvider proves the headline behaviour: two
// aliases with the SAME real id and DIFFERENT only pins produce request
// bodies carrying the SAME model and their OWN provider.only. glm-5.3-flash
// has no static pin, so this also proves an alias pin carries the provider
// field on its own, with nothing to merge from.
func TestSendParamsAliasOnlyPinsProvider(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(aliasPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendAliasParams(t, c, "openrouter/glm-flash@together")
	sendAliasParams(t, c, "openrouter/glm-flash@fireworks")

	ck.Require().Eq(2, openrouter.calls, "openrouter called")
	for i, want := range []string{"together", "fireworks"} {
		body := wireBody(t, openrouter.lastReq, i)
		got := string(openrouter.lastReq[i].Model)
		ck.Eq("z-ai/glm-5.3-flash", got, "request %d: model = %q, want the aliases' shared real id z-ai/glm-5.3-flash", i, got)
		ck.StrContains(body, `"provider":{"only":["`+want+`"]}`, "request %d (%s alias): wire body missing provider.only [%s]", i, want, want)
	}
	// And the two bodies must differ ONLY in the pin, not the model.
	if got := wireBody(t, openrouter.lastReq, 0); !strings.Contains(got, `"only":["together"]`) ||
		strings.Contains(got, `"only":["fireworks"]`) {
		t.Errorf("together alias leaked fireworks (or no pin): %s", got)
	}
	if got := wireBody(t, openrouter.lastReq, 1); !strings.Contains(got, `"only":["fireworks"]`) ||
		strings.Contains(got, `"only":["together"]`) {
		t.Errorf("fireworks alias leaked together (or no pin): %s", got)
	}
}

// TestSendParamsAliasOnlyReplacesStaticPin proves precedence: z-ai/glm-5.2 has
// a static pin (only=["fireworks"]); an alias for the same id with its own
// only replaces it for that request — the alias is the explicit, more
// specific declaration.
func TestSendParamsAliasOnlyReplacesStaticPin(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(aliasPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendAliasParams(t, c, "openrouter/glm52-novita")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["novita"]}`, "alias pin must REPLACE the static pin, wire body =")
	ck.NotStrContains(body, `"fireworks"`, "static pin leaked past the alias pin, wire body =")
}

// TestSendParamsNonAliasUnaffectedByAliasSupport proves a non-alias model
// keeps today's behaviour when the registry declares aliases: no provider
// field for an unpinned, unignored id, and no pin from ANY alias that happens
// to share the provider.
func TestSendParamsNonAliasUnaffectedByAliasSupport(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(aliasPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendAliasParams(t, c, "openrouter/moonshotai/kimi-k3")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.NotStrContains(body, `"provider"`, "non-alias unpinned model must not carry a provider field")
	ck.Eq("moonshotai/kimi-k3", string(openrouter.lastReq[0].Model), "model")
}

// TestSendParamsAliasOnlyMergesGuardIgnore proves the guard's ignore list
// still merges with an alias pin: the alias decides which providers MAY serve,
// the guard still vetoes ones it has ejected.
func TestSendParamsAliasOnlyMergesGuardIgnore(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(aliasPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	// Eject "Together" for the z-ai/glm-5.3-flash line: five qualifying misses
	// (the first turn of a conversation is never evidence).
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{Provider: "Together", Model: "z-ai/glm-5.3-flash",
			Conversation: "c1", PrefixHash: "h1", InputTokens: 50000})
	}
	if ignore := g.IgnoredFor(now, "z-ai/glm-5.3-flash"); len(ignore) != 1 || ignore[0] != "together" {
		t.Fatalf("guard setup failed: IgnoredFor = %v, want [together]", ignore)
	}
	c.SetProviderGuard(g)

	sendAliasParams(t, c, "openrouter/glm-flash@fireworks")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"ignore":["together"]}`, "alias pin + guard ignore not merged as expected, wire body =")
}

// TestSendParamsOperatorBanReachesTheWire proves an operator ban — recorded
// against every model line, not the model being sent — lands in the outgoing
// provider.ignore alongside an alias pin, on a model the guard never observed.
func TestSendParamsOperatorBanReachesTheWire(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(aliasPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	if _, err := g.Ban(context.Background(), time.Now(), "open-inference", 0, ""); err != nil {
		t.Fatal(err)
	}
	c.SetProviderGuard(g)

	sendAliasParams(t, c, "openrouter/glm-flash@fireworks")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"ignore":["open-inference"]}`, "operator ban not merged into the wire body")
}
