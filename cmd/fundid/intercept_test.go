package main

import (
	"encoding/json"
	"testing"
)

func TestInspect_NewSession(t *testing.T) {
	frame := []byte(`{"type":"new_session","id":"x"}`)
	got, ok := inspect(frame)
	if !ok {
		t.Fatal("expected intercept")
	}
	if got.Type != "new_session" || got.PiRequestID != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestInspect_SwitchSession(t *testing.T) {
	frame := []byte(`{"type":"switch_session","id":"y","sessionPath":"/path"}`)
	got, ok := inspect(frame)
	if !ok || got.Type != "switch_session" || got.SessionPath != "/path" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
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
		if ok {
			t.Fatalf("expected no intercept for %q", f)
		}
	}
}

func TestSynthesizeResponse_Shape(t *testing.T) {
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
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "response" || parsed.Command != "new_session" ||
		parsed.ID != "req-1" || !parsed.Success || parsed.Data.Cancelled {
		t.Fatalf("parsed: %+v\nraw: %s", parsed, got)
	}
}
