package child_test

import (
	"testing"

	"graveland.dev/pi-controller/internal/child"
)

func TestSniff_GetStateResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"sid","sessionFile":"/x/s.jsonl","sessionName":"named","model":{"id":"m","provider":"p"}}}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok {
		t.Fatal("expected extraction")
	}
	if md.SessionID != "sid" || md.SessionFile != "/x/s.jsonl" ||
		md.SessionName != "named" || md.Model != "p/m" {
		t.Fatalf("got %+v", md)
	}
}

// Pi's set_session_name response is `{success:true}` with no payload.
// The actual name change arrives via the session_info_changed event below.
func TestSniff_SetSessionNameResponse_NoPayload(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_session_name","success":true}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("set_session_name response without data should not yield metadata")
	}
}

func TestSniff_SessionInfoChangedEvent(t *testing.T) {
	frame := []byte(`{"type":"session_info_changed","name":"renamed"}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok || md.SessionName != "renamed" {
		t.Fatalf("got %+v ok=%v", md, ok)
	}
}

func TestSniff_SessionInfoChangedEvent_EmptyName(t *testing.T) {
	frame := []byte(`{"type":"session_info_changed"}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("empty session_info_changed should not yield metadata")
	}
}

func TestSniff_NonMetadataFrame(t *testing.T) {
	frame := []byte(`{"type":"agent_start"}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("expected no extraction from non-response")
	}
}

func TestSniff_MalformedJson(t *testing.T) {
	frame := []byte(`{not json}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("expected no extraction from invalid JSON")
	}
}

func TestSniff_SetModelResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_model","success":true,"data":{"model":{"id":"opus","provider":"anthropic"}}}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok || md.Model != "anthropic/opus" {
		t.Fatalf("got %+v ok=%v", md, ok)
	}
}

func TestSniff_RejectsSuccessFalse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"get_state","success":false,"error":{"code":"x"}}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("expected no extraction for success:false response")
	}
}
