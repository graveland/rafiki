package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// proxiedClaude is a controller whose claude children are routed through the
// rafiki proxy, which is the shape the proxy's refusals apply to.
func proxiedClaude(t *testing.T, openrouterKey string) *Controller {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", openrouterKey)
	c := &Controller{proxyURL: "http://127.0.0.1:1", proxyToken: "tok"}
	return c
}

func claudeReq(model, passthrough string) protocol.SpawnRequest {
	return protocol.SpawnRequest{Kind: protocol.KindClaude, Model: model, PassthroughAuth: passthrough}
}

func refusal(t *testing.T, err error) *connectapi.ControllerError {
	t.Helper()
	var ce *connectapi.ControllerError
	assert.NewCollecting(t).Require().True(errors.As(err, &ce), "want a ControllerError refusal, got %v", err)
	return ce
}

// The proxy answers a passthrough request for a non-Anthropic model with a 400
// on the child's first turn, and the child sits there. Refuse it at spawn.
func TestClaudeSpawnRefusesPassthroughWithANonAnthropicModel(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := proxiedClaude(t, "or-key")

	err := c.checkClaudeModelRoutable(claudeReq("z-ai/glm-5.3-flash", "on"))

	ck.Require().Error(err, "passthrough on + an OpenRouter model must be refused")
	ce := refusal(t, err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	ck.StrContains(ce.Message, "passthrough", "message")
	ck.StrContains(ce.Message, "z-ai/glm-5.3-flash", "message names the model")
}

// With no OpenRouter key the proxy answers 502 "openrouter not configured",
// which Claude Code retries -- the child looks busy and never answers.
func TestClaudeSpawnRefusesAnOpenRouterModelWithNoOpenRouterKey(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := proxiedClaude(t, "")

	err := c.checkClaudeModelRoutable(claudeReq("openai/gpt-5.6-sol", ""))

	ck.Require().Error(err, "an OpenRouter model with no key must be refused")
	ce := refusal(t, err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	ck.StrContains(ce.Message, "OPENROUTER_API_KEY", "message")
}

// Everything the proxy CAN serve must still spawn.
func TestClaudeSpawnAdmitsWhatTheProxyCanServe(t *testing.T) {
	cases := []struct {
		name, key, model, passthrough string
	}{
		{"openrouter model, key present", "or-key", "openai/gpt-5.6-sol", ""},
		{"openrouter model, passthrough off", "or-key", "openai/gpt-5.6-sol", "off"},
		{"anthropic id, passthrough on, no key", "", "anthropic/claude-opus-5-5", "on"},
		{"dashed anthropic id", "", "claude-opus-5-5", "on"},
		// A bare name routes to Anthropic: Claude Code's own aliases are bare.
		{"bare alias", "", "sonnet", "on"},
		{"no model", "", "", "on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := proxiedClaude(t, tc.key)
			assert.NewCollecting(t).NoError(c.checkClaudeModelRoutable(claudeReq(tc.model, tc.passthrough)), "admit")
		})
	}
}

// The check models the proxy's refusals, so it only applies to children the
// proxy serves, and only to claude.
func TestClaudeModelCheckOnlyAppliesToProxiedClaudeChildren(t *testing.T) {
	ck := assert.NewCollecting(t)

	unproxied := &Controller{}
	t.Setenv("OPENROUTER_API_KEY", "")
	ck.NoError(unproxied.checkClaudeModelRoutable(claudeReq("openai/gpt-5.6-sol", "on")), "no proxy URL: the child talks to whatever base URL is ambient")

	c := proxiedClaude(t, "")
	fundi := claudeReq("openai/gpt-5.6-sol", "on")
	fundi.Kind = protocol.KindFundi
	ck.NoError(c.checkClaudeModelRoutable(fundi), "fundi is not proxied")
}

// Spawn must run the check before anything is created.
func TestSpawnRefusesAnUnroutableClaudeModel(t *testing.T) {
	c := selectFixture(t, "")
	c.proxyURL, c.proxyToken = "http://127.0.0.1:1", "tok"
	t.Setenv("OPENROUTER_API_KEY", "")
	before := len(c.st.List())

	_, err := c.Spawn(context.Background(), claudeReq("openai/gpt-5.6-sol", ""), users.Identity{})

	ck := assert.NewCollecting(t)
	ck.Require().Error(err, "Spawn must refuse")
	ck.StrContains(err.Error(), "OPENROUTER_API_KEY", "refusal")
	ck.Eq(before, len(c.st.List()), "a refused spawn registers no child")
}

// The proxy routes by the shape of the resolved id, not through the provider
// registry: "anthropic/<id>" is stripped to a bare Anthropic id by
// routing.ResolveModel. A providers.toml with no "anthropic" provider must not
// make the check refuse what the proxy serves.
func TestClaudeSpawnAdmitsAnAnthropicIDWithNoAnthropicProvider(t *testing.T) {
	c := proxiedClaude(t, "")
	c.providers = &providers.Set{
		DefaultProvider: "ollama",
		Providers: map[string]providers.Provider{
			"ollama":     {Name: "ollama", Kind: providers.KindOpenAI},
			"openrouter": {Name: "openrouter", Kind: providers.KindAnthropicOpenRouter},
		},
	}

	assert.NewCollecting(t).NoError(c.checkClaudeModelRoutable(claudeReq("anthropic/claude-opus-5-5", "")), "admit")
}
