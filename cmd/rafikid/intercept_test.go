package main

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestInspect_NewSession(t *testing.T) {
	c := assert.NewAborting(t)
	frame := []byte(`{"type":"new_session","id":"x"}`)
	got, ok := inspect(frame)
	c.True(ok, "expected intercept")
	c.False(got.Type != "new_session" || got.PiRequestID != "x", "got %+v", got)
}

func TestInspect_SwitchSession(t *testing.T) {
	frame := []byte(`{"type":"switch_session","id":"y","sessionPath":"/path"}`)
	got, ok := inspect(frame)
	assert.NewAborting(t).False(!ok || got.Type != "switch_session" || got.SessionPath != "/path", "got %+v ok=%v", got, ok)
}

func TestInspect_PassThrough(t *testing.T) {
	for _, f := range []string{
		`{"type":"prompt","message":"hi"}`,
		`{"type":"fork","entryId":"x"}`,
		`{"type":"clone"}`,
		`{"not":"json"}`,
		``,
	} {
		_, ok := inspect([]byte(f))
		assert.NewAborting(t).False(ok, "expected no intercept for %q", f)
	}
}

func TestSynthesizeResponse_Shape(t *testing.T) {
	c := assert.NewAborting(t)
	got := synthesizeResponse("new_session", "req-1")
	var parsed struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Data    struct {
			Cancelled bool `json:"cancelled"`
		} `json:"data"`
	}
	c.NoError(json.Unmarshal(got, &parsed))
	c.False(parsed.Type != "response" || parsed.Command != "new_session" ||
		parsed.ID != "req-1" || !parsed.Success || parsed.Data.Cancelled, "parsed: %+v\nraw: %s", parsed, got)
}
