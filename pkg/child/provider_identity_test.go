package child

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestIdentityProvider_Bootstrap(t *testing.T) {
	got := IdentityProvider{}.BootstrapFrame()
	want := `{"type":"get_state","id":"__bootstrap__"}`
	assert.NewAborting(t).Eq(want, string(got), "bootstrap = %q, want", got)
}

func TestIdentityProvider_EncodeOutboundIsIdentity(t *testing.T) {
	in := []byte(`{"type":"prompt","message":"hi"}`)
	got := IdentityProvider{}.EncodeOutbound(in)
	assert.NewAborting(t).Eq(string(in), string(got), "encode = %q, want identity %q", got, in)
}

func TestIdentityProvider_Parse_GetStateFirstResponseAndMeta(t *testing.T) {
	c := assert.NewAborting(t)
	line := []byte(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"s1","sessionFile":"/tmp/s.jsonl","sessionName":"alpha","model":{"id":"opus","provider":"anthropic"}}}`)
	res := IdentityProvider{}.Parse(line)
	c.True(res.FirstResponse, "expected FirstResponse on get_state response")
	c.False(!res.HasMeta || res.Meta.SessionID != "s1" || res.Meta.Model != "anthropic/opus", "meta = %+v hasMeta=%v", res.Meta, res.HasMeta)
	c.Empty(res.Events, "get_state response should emit no SM events, got")
}

func TestIdentityProvider_Parse_AgentStartEvent(t *testing.T) {
	c := assert.NewAborting(t)
	res := IdentityProvider{}.Parse([]byte(`{"type":"agent_start"}`))
	c.False(res.FirstResponse, "agent_start must not be FirstResponse")
	c.False(len(res.Events) != 1 || res.Events[0].Type != "agent_start", "events = %+v", res.Events)
}

func TestIdentityProvider_Parse_AutoRetryCarriesError(t *testing.T) {
	res := IdentityProvider{}.Parse([]byte(`{"type":"auto_retry_start","errorMessage":"429 overloaded"}`))
	assert.NewAborting(t).False(len(res.Events) != 1 || res.Events[0].Type != "auto_retry_start" || res.Events[0].RetryError != "429 overloaded", "events = %+v", res.Events)
}

func TestIdentityProvider_Parse_UIRequestCarriesMeta(t *testing.T) {
	res := IdentityProvider{}.Parse([]byte(`{"type":"extension_ui_request","id":"u1","method":"confirm"}`))
	assert.NewAborting(t).False(len(res.Events) != 1 || res.Events[0].Type != "extension_ui_request" || res.Events[0].UI == nil || res.Events[0].UI.ID != "u1" || res.Events[0].UI.Method != "confirm", "events = %+v", res.Events)
}

func TestIdentityProvider_Parse_Garbage(t *testing.T) {
	res := IdentityProvider{}.Parse([]byte(`not json`))
	assert.NewAborting(t).False(res.FirstResponse || res.HasMeta || len(res.Events) != 0, "garbage should be a no-op, got %+v", res)
}
