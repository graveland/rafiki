package child

import "testing"

func TestPiProvider_Bootstrap(t *testing.T) {
	got := PiProvider{}.BootstrapFrame()
	want := `{"type":"get_state","id":"__bootstrap__"}`
	if string(got) != want {
		t.Fatalf("bootstrap = %q, want %q", got, want)
	}
}

func TestPiProvider_EncodeOutboundIsIdentity(t *testing.T) {
	in := []byte(`{"type":"prompt","message":"hi"}`)
	got := PiProvider{}.EncodeOutbound(in)
	if string(got) != string(in) {
		t.Fatalf("encode = %q, want identity %q", got, in)
	}
}

func TestPiProvider_Parse_GetStateFirstResponseAndMeta(t *testing.T) {
	line := []byte(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"s1","sessionFile":"/tmp/s.jsonl","sessionName":"alpha","model":{"id":"opus","provider":"anthropic"}}}`)
	res := PiProvider{}.Parse(line)
	if !res.FirstResponse {
		t.Fatal("expected FirstResponse on get_state response")
	}
	if !res.HasMeta || res.Meta.SessionID != "s1" || res.Meta.Model != "anthropic/opus" {
		t.Fatalf("meta = %+v hasMeta=%v", res.Meta, res.HasMeta)
	}
	if len(res.Events) != 0 {
		t.Fatalf("get_state response should emit no SM events, got %+v", res.Events)
	}
}

func TestPiProvider_Parse_AgentStartEvent(t *testing.T) {
	res := PiProvider{}.Parse([]byte(`{"type":"agent_start"}`))
	if res.FirstResponse {
		t.Fatal("agent_start must not be FirstResponse")
	}
	if len(res.Events) != 1 || res.Events[0].Type != "agent_start" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestPiProvider_Parse_AutoRetryCarriesError(t *testing.T) {
	res := PiProvider{}.Parse([]byte(`{"type":"auto_retry_start","errorMessage":"429 overloaded"}`))
	if len(res.Events) != 1 || res.Events[0].Type != "auto_retry_start" || res.Events[0].RetryError != "429 overloaded" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestPiProvider_Parse_UIRequestCarriesMeta(t *testing.T) {
	res := PiProvider{}.Parse([]byte(`{"type":"extension_ui_request","id":"u1","method":"confirm"}`))
	if len(res.Events) != 1 || res.Events[0].Type != "extension_ui_request" || res.Events[0].UI == nil || res.Events[0].UI.ID != "u1" || res.Events[0].UI.Method != "confirm" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestPiProvider_Parse_Garbage(t *testing.T) {
	res := PiProvider{}.Parse([]byte(`not json`))
	if res.FirstResponse || res.HasMeta || len(res.Events) != 0 {
		t.Fatalf("garbage should be a no-op, got %+v", res)
	}
}
