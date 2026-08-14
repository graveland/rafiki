// SPDX-License-Identifier: Apache-2.0

package rawtrace

import (
	"encoding/json"
	"testing"
)

func TestNilJSON(t *testing.T) {
	// Valid JSON passes through unchanged (the common case: headers, JSON bodies).
	valid := json.RawMessage(`{"model":"claude-sonnet-5"}`)
	got := nilJSON(valid)
	b, ok := got.([]byte)
	if !ok {
		if rm, ok2 := got.(json.RawMessage); ok2 {
			b = rm
		} else {
			t.Fatalf("expected []byte/json.RawMessage passthrough, got %T", got)
		}
	}
	if string(b) != string(valid) {
		t.Errorf("valid JSON was altered: got %q, want %q", b, valid)
	}

	// Non-JSON (e.g. an accumulated text/event-stream SSE body, or an HTML error
	// page from a load balancer) must be wrapped as a JSON string so the INSERT
	// into a JSONB column can never fail with "invalid input syntax for type json".
	sse := json.RawMessage("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	wrapped := nilJSON(sse)
	wb, ok := wrapped.([]byte)
	if !ok {
		t.Fatalf("expected []byte for wrapped non-JSON payload, got %T", wrapped)
	}
	if !json.Valid(wb) {
		t.Fatalf("wrapped payload is not valid JSON: %s", wb)
	}
	var s string
	if err := json.Unmarshal(wb, &s); err != nil {
		t.Fatalf("wrapped payload did not unmarshal as a JSON string: %v", err)
	}
	if s != string(sse) {
		t.Errorf("wrapped payload lost data: got %q, want %q", s, sse)
	}

	// Empty input maps to SQL NULL.
	if got := nilJSON(nil); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := nilJSON(json.RawMessage{}); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}
