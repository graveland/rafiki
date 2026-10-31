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

// sdkStreamErr builds the error ssestream v1.37.0 produces for an in-band
// "error" event: a bare fmt.Errorf over the raw event body. The SDK-typed fakes
// still surface a stream failure this way, and the compat adapter recovers the
// payload from it — so the literal stays here as the wire contract.
func sdkStreamErr(payload string) error {
	return fmt.Errorf("received error while streaming: %s", payload)
}

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

// TestParseStreamError_IsTransient covers the real payloads seen on the wire,
// pinned from live OpenRouter traffic (the gemini timeout_error abort) and
// Anthropic's documented in-band error shapes.
func TestParseStreamError_IsTransient(t *testing.T) {
	tests := []struct {
		name      string
		kind      msg.ErrorKind
		payload   string
		want      StreamError
		wantTrans bool
		wantRL    bool
	}{
		{
			name: "openrouter upstream rate limit (live payload)",
			kind: msg.ErrRateLimit,
			payload: `{"type":"error","error":{"type":"rate_limit_error",` +
				`"message":"Provider returned error","error_type":"rate_limit_exceeded"}}`,
			want: StreamError{Type: "error", ErrType: "rate_limit_error", Message: "Provider returned error"},
			// A rate limit takes the ModelGate path instead of agentloop's
			// generic backoff.
			wantTrans: false,
			wantRL:    true,
		},
		{
			name:    "openrouter-native rate_limit_exceeded spelling",
			kind:    msg.ErrRateLimit,
			payload: `{"type":"error","error":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			want:    StreamError{Type: "error", ErrType: "rate_limit_exceeded", Message: "slow down"},
			wantRL:  true,
		},
		{
			name:    "bare inner rate-limit envelope",
			kind:    msg.ErrRateLimit,
			payload: `{"type":"rate_limit_error","message":"quota exhausted"}`,
			want:    StreamError{Type: "rate_limit_error", ErrType: "rate_limit_error", Message: "quota exhausted"},
			wantRL:  true,
		},
		{
			name: "openrouter upstream timeout (live payload)",
			kind: msg.ErrTransient,
			payload: `{"type":"error","error":{"type":"timeout_error",` +
				`"message":"The operation was aborted","error_type":"timeout"}}`,
			want:      StreamError{Type: "error", ErrType: "timeout_error", Message: "The operation was aborted"},
			wantTrans: true,
		},
		{
			name:      "overloaded is transient",
			kind:      msg.ErrOverloaded,
			payload:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:      StreamError{Type: "error", ErrType: "overloaded_error", Message: "Overloaded"},
			wantTrans: true,
		},
		{
			name:      "anthropic api_error",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			want:      StreamError{Type: "error", ErrType: "api_error", Message: "Internal server error"},
			wantTrans: true,
		},
		{
			name:      "bare inner envelope (no error object)",
			kind:      msg.ErrTransient,
			payload:   `{"type":"timeout_error","message":"upstream timeout"}`,
			want:      StreamError{Type: "timeout_error", ErrType: "timeout_error", Message: "upstream timeout"},
			wantTrans: true,
		},
		{
			name:      "numeric code >= 500 with no type",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"code":504,"message":"gateway timeout"}}`,
			want:      StreamError{Type: "error", Code: 504, Message: "gateway timeout"},
			wantTrans: true,
		},
		{
			name:      "string-encoded numeric code >= 500",
			kind:      msg.ErrTransient,
			payload:   `{"type":"error","error":{"code":"502","message":"bad gateway"}}`,
			want:      StreamError{Type: "error", Code: 502, Message: "bad gateway"},
			wantTrans: true,
		},
		{
			name:    "invalid_request_error is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`,
			want:    StreamError{Type: "error", ErrType: "invalid_request_error", Message: "messages: field required"},
		},
		{
			name:    "authentication_error is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`,
			want:    StreamError{Type: "error", ErrType: "authentication_error", Message: "invalid api key"},
		},
		{
			name:    "billing-ish 4xx code is not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"code":401,"message":"User not found."}}`,
			want:    StreamError{Type: "error", Code: 401, Message: "User not found."},
		},
		{
			name:    "unknown type defaults to not transient",
			kind:    msg.ErrOther,
			payload: `{"type":"error","error":{"type":"weird_new_error","message":"?"}}`,
			want:    StreamError{Type: "error", ErrType: "weird_new_error", Message: "?"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := inBandErr(tt.kind, tt.payload)
			se, ok := ParseStreamError(err)
			c.Require().True(ok, "ParseStreamError ok")
			c.Eq(tt.want, se, "parsed")
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
		name      string
		err       error
		wantParse bool
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
			_, ok := ParseStreamError(tt.err)
			c.Eq(tt.wantParse, ok, "ParseStreamError ok")
			c.False(IsTransientStreamError(tt.err), "IsTransientStreamError(%v) = true, want false", tt.err)
			c.False(IsRateLimitStreamError(tt.err), "IsRateLimitStreamError(%v) = true, want false", tt.err)
		})
	}
}
