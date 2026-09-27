// SPDX-License-Identifier: Apache-2.0

package rawtrace

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestNilJSON(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Eq(string(valid), string(b), "valid JSON was altered: got %q, want %q", b, valid)

	// Non-JSON (e.g. an accumulated text/event-stream SSE body, or an HTML error
	// page from a load balancer) must be wrapped as a JSON string so the INSERT
	// into a JSONB column can never fail with "invalid input syntax for type json".
	sse := json.RawMessage("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	wrapped := nilJSON(sse)
	wb, ok := wrapped.([]byte)
	c.Require().True(ok, "expected []byte for wrapped non-JSON payload, got %T", wrapped)
	c.Require().True(json.Valid(wb), "wrapped payload is not valid JSON: %s", wb)
	var s string
	c.Require().NoError(json.Unmarshal(wb, &s), "wrapped payload did not unmarshal as a JSON string")
	c.Eq(string(sse), s, "wrapped payload lost data: got %q, want %q", s, sse)

	// Empty input maps to SQL NULL.
	c.Nil(nilJSON(nil), "expected nil for empty input, got")
	c.Nil(nilJSON(json.RawMessage{}), "expected nil for empty input, got")
}
