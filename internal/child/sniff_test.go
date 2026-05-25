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

func TestSniff_SetSessionNameResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_session_name","success":true,"data":{"name":"renamed"}}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok || md.SessionName != "renamed" {
		t.Fatalf("got %+v ok=%v", md, ok)
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
