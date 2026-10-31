// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// declPinSet builds the two-declarations-one-id registry through the REAL
// config path (providers.Parse), so the test also proves the toml `only` key
// round-trips: a declaration pin is declared per declaration key, never per
// model id.
const declPinTOML = `
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

func declPinSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(declPinTOML))
	assert.NewAborting(t).NoError(err, "Parse declaration-pin providers.toml")
	return set
}

// sendDecl sends one non-streaming turn for model and returns nothing;
// failures fail the test.
func sendDecl(t *testing.T, c *Client, model string) {
	t.Helper()
	_, err := c.send(context.Background(), SendMeta{}, userTextRequest(model), nil)
	assert.NewAborting(t).NoError(err, "send(%s)", model)
}

// wireBody encodes one captured request the way the wire sees it, including
// the caller's extras ("provider").
func wireBody(t *testing.T, s *scriptedSender, i int) string {
	t.Helper()
	b, err := msganthropic.Encode(s.lastReq[i], s.lastOpts[i])
	assert.NewAborting(t).NoError(err, "encode wire body")
	return string(b)
}

// TestSendDeclOnlyPinsProvider proves the headline behaviour: two
// declarations with the SAME real id and DIFFERENT only pins produce request
// bodies carrying the SAME model and their OWN provider.only. Nothing else
// supplies a pin, so this also proves a declaration pin carries the provider
// field on its own, with nothing to merge from.
func TestSendDeclOnlyPinsProvider(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(declPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendDecl(t, c, "openrouter/glm-flash@together")
	sendDecl(t, c, "openrouter/glm-flash@fireworks")

	ck.Require().Eq(2, openrouter.calls, "openrouter called")
	for i, want := range []string{"together", "fireworks"} {
		body := wireBody(t, openrouter, i)
		got := openrouter.lastReq[i].Model
		ck.Eq("z-ai/glm-5.3-flash", got, "request %d: model = %q, want the declarations' shared real id z-ai/glm-5.3-flash", i, got)
		ck.StrContains(body, `"provider":{"only":["`+want+`"]}`, "request %d (%s declaration): wire body missing provider.only [%s]", i, want, want)
	}
	// And the two bodies must differ ONLY in the pin, not the model.
	if got := wireBody(t, openrouter, 0); !strings.Contains(got, `"only":["together"]`) ||
		strings.Contains(got, `"only":["fireworks"]`) {
		t.Errorf("together declaration leaked fireworks (or no pin): %s", got)
	}
	if got := wireBody(t, openrouter, 1); !strings.Contains(got, `"only":["fireworks"]`) ||
		strings.Contains(got, `"only":["together"]`) {
		t.Errorf("fireworks declaration leaked together (or no pin): %s", got)
	}
}

// TestSendDeclarationOnlyIsTheOnlyPin proves the declaration's only is
// the sole source of a pin: the same real id requested WITHOUT a declaration
// carries no provider object at all (nothing supplies a built-in per-line
// pin), while the declared key's only reaches the wire unchanged.
func TestSendDeclarationOnlyIsTheOnlyPin(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"), respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(declPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendDecl(t, c, "openrouter/glm52-novita")
	sendDecl(t, c, "openrouter/z-ai/glm-5.2")

	body := wireBody(t, openrouter, 0)
	ck.StrContains(body, `"provider":{"only":["novita"]}`, "the declaration's only must reach the wire, wire body =")
	raw := wireBody(t, openrouter, 1)
	ck.NotStrContains(raw, `"provider"`, "an undeclared id must carry no provider object, wire body =")
	ck.NotStrContains(raw, `"fireworks"`, "no built-in pin may reappear, wire body =")
}

// TestSendUndeclaredUnaffectedByDeclarations proves an undeclared model
// keeps today's behaviour when the registry declares models: no provider
// field for an unpinned, unignored id, and no pin from ANY declaration that
// happens to share the provider.
func TestSendUndeclaredUnaffectedByDeclarations(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(declPinSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	sendDecl(t, c, "openrouter/moonshotai/kimi-k3")

	body := wireBody(t, openrouter, 0)
	ck.NotStrContains(body, `"provider"`, "undeclared unpinned model must not carry a provider field")
	ck.Eq("moonshotai/kimi-k3", openrouter.lastReq[0].Model, "model")
}

// TestSendDeclOnlyMergesGuardIgnore proves the guard's ignore list
// still merges with a declaration pin: the declaration decides which providers
// MAY serve, the guard still vetoes ones it has ejected.
func TestSendDeclOnlyMergesGuardIgnore(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(declPinSet(t)),
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

	sendDecl(t, c, "openrouter/glm-flash@fireworks")

	body := wireBody(t, openrouter, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"ignore":["together"]}`, "declaration pin + guard ignore not merged as expected, wire body =")
}

// TestSendOperatorBanReachesTheWire proves an operator ban — recorded
// against every model line, not the model being sent — lands in the outgoing
// provider.ignore alongside a declaration pin, on a model the guard never
// observed.
func TestSendOperatorBanReachesTheWire(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviders(declPinSet(t)),
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

	sendDecl(t, c, "openrouter/glm-flash@fireworks")

	body := wireBody(t, openrouter, 0)
	ck.StrContains(body, `"provider":{"only":["fireworks"],"ignore":["open-inference"]}`, "operator ban not merged into the wire body")
}
