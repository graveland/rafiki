// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// sdkStreamErr builds the error exactly as ssestream v1.37.0 does for an
// in-band "error" event: a bare fmt.Errorf over the raw event body. Keeping
// the literal here (not via the unexported prefix) guards against the
// classifier and the SDK drifting apart silently — the string IS the wire
// contract.
func sdkStreamErr(payload string) error {
	return fmt.Errorf("received error while streaming: %s", payload)
}

// TestParseStreamError_IsTransient covers the real payloads seen on the wire,
// pinned from live OpenRouter traffic (the gemini timeout_error abort) and
// Anthropic's documented in-band error shapes.
func TestParseStreamError_IsTransient(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantParse bool
		want      StreamError
		wantTrans bool
		wantRL    bool
	}{
		{
			name: "openrouter upstream rate limit (live payload)",
			err: sdkStreamErr(`{"type":"error","error":{"type":"rate_limit_error",` +
				`"message":"Provider returned error","error_type":"rate_limit_exceeded"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "rate_limit_error", Message: "Provider returned error"},
			// Transient too, but deliberately classified as a RATE LIMIT so it
			// takes the ModelGate path instead of agentloop's generic backoff.
			wantTrans: false,
			wantRL:    true,
		},
		{
			name:      "openrouter-native rate_limit_exceeded spelling",
			err:       sdkStreamErr(`{"type":"error","error":{"type":"rate_limit_exceeded","message":"slow down"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "rate_limit_exceeded", Message: "slow down"},
			wantTrans: false,
			wantRL:    true,
		},
		{
			name:      "bare inner rate-limit envelope",
			err:       sdkStreamErr(`{"type":"rate_limit_error","message":"quota exhausted"}`),
			wantParse: true,
			want:      StreamError{Type: "rate_limit_error", ErrType: "rate_limit_error", Message: "quota exhausted"},
			wantTrans: false,
			wantRL:    true,
		},
		{
			name: "openrouter upstream timeout (live payload)",
			err: sdkStreamErr(`{"type":"error","error":{"type":"timeout_error",` +
				`"message":"The operation was aborted","error_type":"timeout"}}`),
			wantParse: true,
			want: StreamError{Type: "error", ErrType: "timeout_error",
				Message: "The operation was aborted"},
			wantTrans: true,
		},
		{
			name:      "wrapped by an outer caller still classifies",
			err:       fmt.Errorf("llm: iteration 14: %w", sdkStreamErr(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "overloaded_error", Message: "Overloaded"},
			wantTrans: true,
		},
		{
			name:      "anthropic api_error",
			err:       sdkStreamErr(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "api_error", Message: "Internal server error"},
			wantTrans: true,
		},
		{
			name:      "bare inner envelope (no error object)",
			err:       sdkStreamErr(`{"type":"timeout_error","message":"upstream timeout"}`),
			wantParse: true,
			want:      StreamError{Type: "timeout_error", ErrType: "timeout_error", Message: "upstream timeout"},
			wantTrans: true,
		},
		{
			name:      "numeric code >= 500 with no type",
			err:       sdkStreamErr(`{"type":"error","error":{"code":504,"message":"gateway timeout"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", Code: 504, Message: "gateway timeout"},
			wantTrans: true,
		},
		{
			name:      "string-encoded numeric code >= 500",
			err:       sdkStreamErr(`{"type":"error","error":{"code":"502","message":"bad gateway"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", Code: 502, Message: "bad gateway"},
			wantTrans: true,
		},
		{
			name:      "invalid_request_error is not transient",
			err:       sdkStreamErr(`{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "invalid_request_error", Message: "messages: field required"},
			wantTrans: false,
		},
		{
			name:      "authentication_error is not transient",
			err:       sdkStreamErr(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "authentication_error", Message: "invalid api key"},
			wantTrans: false,
		},
		{
			name:      "billing-ish 4xx code is not transient",
			err:       sdkStreamErr(`{"type":"error","error":{"code":401,"message":"User not found."}}`),
			wantParse: true,
			want:      StreamError{Type: "error", Code: 401, Message: "User not found."},
			wantTrans: false,
		},
		{
			name:      "unknown type defaults to not transient",
			err:       sdkStreamErr(`{"type":"error","error":{"type":"weird_new_error","message":"?"}}`),
			wantParse: true,
			want:      StreamError{Type: "error", ErrType: "weird_new_error", Message: "?"},
			wantTrans: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			se, ok := ParseStreamError(tt.err)
			if ok != tt.wantParse {
				t.Fatalf("ParseStreamError ok = %v, want %v", ok, tt.wantParse)
			}
			if se != tt.want {
				t.Errorf("parsed = %+v, want %+v", se, tt.want)
			}
			if got := IsTransientStreamError(tt.err); got != tt.wantTrans {
				t.Errorf("IsTransientStreamError = %v, want %v", got, tt.wantTrans)
			}
			if got := IsRateLimitStreamError(tt.err); got != tt.wantRL {
				t.Errorf("IsRateLimitStreamError = %v, want %v", got, tt.wantRL)
			}
		})
	}
}

// TestIsTransientStreamError_OtherErrors pins the classifiers' blindness to
// everything that is NOT the ssestream wrapper: typed SDK errors, network
// errors and plain strings must stay false so no existing retry path changes
// behavior.
func TestIsTransientStreamError_OtherErrors(t *testing.T) {
	apiErr := &anthropic.Error{StatusCode: http.StatusBadGateway}
	if err := apiErr.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"api_error","message":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("something broke")},
		{"typed 502 without wrapper", apiErr},
		{"typed 429 without wrapper", rateLimitErr()},
		{"net-style error", errors.New("read tcp: connection reset by peer")},
		{"wrapper with non-JSON body", sdkStreamErr(`<html>502 Bad Gateway</html>`)},
		{"wrapper with empty body", sdkStreamErr("")},
		{"wrapper with JSON array", sdkStreamErr(`[1,2,3]`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := ParseStreamError(tt.err); ok {
				t.Errorf("ParseStreamError reported ok for %v", tt.err)
			}
			if IsTransientStreamError(tt.err) {
				t.Errorf("IsTransientStreamError(%v) = true, want false", tt.err)
			}
			if IsRateLimitStreamError(tt.err) {
				t.Errorf("IsRateLimitStreamError(%v) = true, want false", tt.err)
			}
		})
	}
}
