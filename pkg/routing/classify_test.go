// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// apiError builds the native refusal the classifiers read: a status, the body
// the provider sent, and the kind the codec derived from it.
func apiError(status int, body string, kind msg.ErrorKind) *msg.APIError {
	return &msg.APIError{Status: status, Body: []byte(body), Kind: kind}
}

func TestRetryableExcludesContextCancellation(t *testing.T) {
	c := assert.NewCollecting(t)
	// Context cancellation should not be retryable.
	c.False(retryable(context.Canceled), "retryable(context.Canceled) = true, want false")
	c.False(retryable(context.DeadlineExceeded), "retryable(context.DeadlineExceeded) = true, want false")
	// But 5xx errors should still be retryable.
	c.True(retryable(apiError(529, "", msg.ErrOverloaded)), "retryable(529) = false, want true")
}

// The real Anthropic body for an exhausted account, verbatim.
const creditBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`

func TestCreditExhausted(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"anthropic 400", 400, creditBody, true},
		{"openrouter 402", 402, `{"error":{"message":"Insufficient credits"}}`, true},
		{"openai-shaped 429 is a rate limit, not billing", 429, `{"error":{"code":"insufficient_quota"}}`, false},
		{"ordinary 400", 400, `{"error":{"message":"max_tokens: must be >= 1"}}`, false},
		{"prompt echoed in an unrelated 413 must not match", 413, creditBody, false},
		{"success", 200, "", false},
		{"empty body", 400, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CreditExhausted(tt.status, []byte(tt.body))
			assert.NewCollecting(t).Eq(tt.want, got, "CreditExhausted(%d, %q) = %v, want", tt.status, tt.body, got)
		})
	}
}

// An out-of-credit account is NOT retryable (the same request fails
// identically) but IS failover-worthy — the whole point of the distinction.
func TestFailoverWorthyIncludesCreditExhaustion(t *testing.T) {
	c := assert.NewCollecting(t)
	err := apiError(400, creditBody, msg.ErrOther)
	c.False(retryable(err), "retryable(credit-exhausted) = true, want false (retrying it is pointless)")
	c.True(FailoverWorthy(err), "FailoverWorthy(credit-exhausted) = false, want true")
	// An ordinary 400 stays non-failover-worthy: failing over would just
	// re-send a malformed request to a second provider.
	bad := apiError(400, `{"error":{"message":"max_tokens: must be >= 1"}}`, msg.ErrOther)
	c.False(FailoverWorthy(bad), "FailoverWorthy(ordinary 400) = true, want false")
	c.False(FailoverWorthy(nil), "FailoverWorthy(nil) = true, want false")
	c.True(FailoverWorthy(apiError(529, "", msg.ErrOverloaded)), "FailoverWorthy(529) = false, want true")
}

// TestRetryableInBand pins the status-0 rule: an in-band stream error has no
// status to classify, so its KIND decides — an overload or a transient upstream
// failure is worth re-issuing, and anything else (a rate limit, which has its
// own backpressure path, or an unclassified error) is not.
func TestRetryableInBand(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		kind msg.ErrorKind
		want bool
	}{
		{"transient", msg.ErrTransient, true},
		{"overloaded", msg.ErrOverloaded, true},
		{"rate limit", msg.ErrRateLimit, false},
		{"unclassified", msg.ErrOther, false},
		{"prompt too large", msg.ErrPromptTooLarge, false},
	} {
		c.Eq(tc.want, retryable(apiError(0, "", tc.kind)), "%s", tc.name)
	}
	c.False(FailoverWorthy(apiError(0, "", msg.ErrRateLimit)),
		"an in-band rate limit is backpressure, not a failover signal")
}
