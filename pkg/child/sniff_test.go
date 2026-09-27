package child_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/child"

	"github.com/multigres/testkit/assert"
)

func TestSniff_GetStateResponse(t *testing.T) {
	c := assert.NewAborting(t)
	frame := []byte(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"sid","sessionFile":"/x/s.jsonl","sessionName":"named","model":{"id":"m","provider":"p"}}}`)
	md, ok := child.ExtractMetadata(frame)
	c.True(ok, "expected extraction")
	c.False(md.SessionID != "sid" || md.SessionFile != "/x/s.jsonl" ||
		md.SessionName != "named" || md.Model != "p/m", "got %+v", md)
}

// Pi's set_session_name response is `{success:true}` with no payload.
// The actual name change arrives via the session_info_changed event below.
func TestSniff_SetSessionNameResponse_NoPayload(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_session_name","success":true}`)
	_, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(ok, "set_session_name response without data should not yield metadata")
}

func TestSniff_SessionInfoChangedEvent(t *testing.T) {
	frame := []byte(`{"type":"session_info_changed","name":"renamed"}`)
	md, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(!ok || md.SessionName != "renamed", "got %+v ok=%v", md, ok)
}

func TestSniff_SessionInfoChangedEvent_EmptyName(t *testing.T) {
	frame := []byte(`{"type":"session_info_changed"}`)
	_, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(ok, "empty session_info_changed should not yield metadata")
}

func TestSniff_NonMetadataFrame(t *testing.T) {
	frame := []byte(`{"type":"agent_start"}`)
	_, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(ok, "expected no extraction from non-response")
}

func TestSniff_MalformedJson(t *testing.T) {
	frame := []byte(`{not json}`)
	_, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(ok, "expected no extraction from invalid JSON")
}

func TestSniff_SetModelResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_model","success":true,"data":{"model":{"id":"opus","provider":"anthropic"}}}`)
	md, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(!ok || md.Model != "anthropic/opus", "got %+v ok=%v", md, ok)
}

func TestSniff_RejectsSuccessFalse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"get_state","success":false,"error":{"code":"x"}}`)
	_, ok := child.ExtractMetadata(frame)
	assert.NewAborting(t).False(ok, "expected no extraction for success:false response")
}
