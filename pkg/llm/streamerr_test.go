// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// inBandErr builds the status-0 API error a codec produces for an in-band SSE
// error event. The KIND is the codec's classification, which its own tests pin;
// this file pins what the classifiers here do with it.
func inBandErr(kind msg.ErrorKind, payload string) *msg.APIError {
	return &msg.APIError{
		Status:  0,
		Body:    []byte(payload),
		Kind:    kind,
		Dialect: msg.DialectAnthropic,
	}
}

// TestIsTransientStreamError covers the real payloads seen on the wire, pinned
// from live OpenRouter traffic (the gemini timeout_error abort) and Anthropic's
// documented in-band error shapes. Only the codec's classification matters
// here; parsing the payload is the codec's own test's business.
func TestIsTransientStreamError(t *testing.T) {
	tests := []struct {
		name      string
		kind      msg.ErrorKind
		payload   string
		wantTrans bool
		wantRL    bool
	}{
		{
			name: "openrouter upstream rate limit (live payload)",
			kind: msg.ErrRateLimit,
			payload: `{"type":"error","error":{"type":"rate_limit_error",` +
				`"message":"Provider returned error","error_type":"rate_limit_exceeded"}}`,
			// A rate limit takes the ModelGate path instead of agentloop's
			// generic backoff.
			wantTrans: false,
			wantRL:    true,
		},
		{
			name:    "openrouter-native rate_limit_exceeded spelling",
			kind:    msg.ErrRateLimit,
			payload: `{"type":"error","error":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			wantRL:  true,
		},
		{
			name:    "bare inner rate-limit envelope",
			kind:    msg.ErrRateLimit,
			payload: `{"type":"rate_limit_error","message":"quota exhausted"}`,
			wantRL:  true,
		},
		{
			name: "openrouter upstream timeout (live payload)",
			kind: msg.ErrTransient,
			payload: `{"type":"error","error":{"type":"timeout_error",` +
				`"message":"The operation was aborted","error_type":"timeout"}}`,
			wantTrans: true,
		},
		{
			name:      "overloaded is transient",
			kind:      msg.ErrOverloaded,
			payload:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantTrans: true,
		},
		{
			name:      "anthropic api_error",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			wantTrans: true,
		},
		{
			name:      "bare inner envelope (no error object)",
			kind:      msg.ErrTransient,
			payload:   `{"type":"timeout_error","message":"upstream timeout"}`,
			wantTrans: true,
		},
		{
			name:      "numeric code >= 500 with no type",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"code":504,"message":"gateway timeout"}}`,
			wantTrans: true,
		},
		{
			name:      "string-encoded numeric code >= 500",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"code":"502","message":"bad gateway"}}`,
			wantTrans: true,
		},
		{
			name:    "invalid_request_error is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`,
		},
		{
			name:    "authentication_error is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`,
		},
		{
			name:    "billing-ish 4xx code is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"code":401,"message":"User not found."}}`,
		},
		{
			name:    "unknown type defaults to not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"weird_new_error","message":"?"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := inBandErr(tt.kind, tt.payload)
			c.Eq(tt.wantTrans, IsTransientStreamError(err), "IsTransientStreamError")
			c.Eq(tt.wantRL, IsRateLimitStreamError(err), "IsRateLimitStreamError")
			// Wrapping never changes the classification.
			wrapped := fmt.Errorf("llm: iteration 14: %w", err)
			c.Eq(tt.wantTrans, IsTransientStreamError(wrapped), "IsTransientStreamError (wrapped)")
			c.Eq(tt.wantRL, IsRateLimitStreamError(wrapped), "IsRateLimitStreamError (wrapped)")
		})
	}
}

// TestIsTransientStreamError_OtherErrors pins the classifiers' blindness to
// everything that is NOT an in-band error: a status-carrying APIError, network
// errors and plain strings must stay false so no existing retry path changes
// behaviour.
func TestIsTransientStreamError_OtherErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantInBand bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("something broke"), false},
		{"net-style error", errors.New("read tcp: connection reset by peer"), false},
		{
			"status-carrying 502 is an HTTP refusal, not a stream event",
			&msg.APIError{Status: http.StatusBadGateway, Kind: msg.ErrTransient},
			false,
		},
		{
			"status-carrying 429 is not an in-band rate limit",
			&msg.APIError{Status: http.StatusTooManyRequests, Kind: msg.ErrRateLimit},
			false,
		},
		{
			// It IS an in-band error (status 0); its body simply names nothing.
			"in-band with a non-JSON body names nothing",
			inBandErr(msg.ErrOther, `<html>502 Bad Gateway</html>`),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, inBand := inBandAPIError(tt.err)
			c.Eq(tt.wantInBand, inBand, "in-band error")
			c.False(IsTransientStreamError(tt.err), "IsTransientStreamError(%v) = true, want false", tt.err)
			c.False(IsRateLimitStreamError(tt.err), "IsRateLimitStreamError(%v) = true, want false", tt.err)
		})
	}
}
