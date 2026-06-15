package main

import "testing"

func TestInnerEvent(t *testing.T) {
	// A ctrl_event envelope (live shape) → returns the inner event bytes verbatim.
	env := []byte(`{"type":"ctrl_event","childId":"c1","event":{"type":"message_end","x":1}}`)
	if got, want := string(innerEvent(env)), `{"type":"message_end","x":1}`; got != want {
		t.Fatalf("innerEvent(envelope) = %s, want %s", got, want)
	}
	// A raw inner frame (backfill shape) → returned unchanged.
	raw := []byte(`{"type":"message_end","x":1}`)
	if got := string(innerEvent(raw)); got != string(raw) {
		t.Fatalf("innerEvent(raw) = %s, want unchanged", got)
	}
	// Non-ctrl_event envelope (e.g. lifecycle) → returned unchanged.
	life := []byte(`{"type":"ctrl_child_exited","childId":"c1","exitCode":0}`)
	if got := string(innerEvent(life)); got != string(life) {
		t.Fatalf("innerEvent(lifecycle) = %s, want unchanged", got)
	}
	// A ctrl_event envelope with no inner event → returned unchanged.
	noEvent := []byte(`{"type":"ctrl_event","childId":"c1"}`)
	if got := string(innerEvent(noEvent)); got != string(noEvent) {
		t.Fatalf("innerEvent(no-event) = %s, want unchanged", got)
	}
	// Malformed JSON → returned unchanged.
	bad := []byte(`{not json`)
	if got := string(innerEvent(bad)); got != string(bad) {
		t.Fatalf("innerEvent(malformed) = %s, want unchanged", got)
	}
}
