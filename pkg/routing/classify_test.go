// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"
)

func TestRetryableExcludesContextCancellation(t *testing.T) {
	c := assert.NewCollecting(t)
	// Context cancellation should not be retryable.
	c.False(retryable(context.Canceled), "retryable(context.Canceled) = true, want false")
	c.False(retryable(context.DeadlineExceeded), "retryable(context.DeadlineExceeded) = true, want false")
	// But 5xx errors should still be retryable.
	c.True(retryable(&anthropic.Error{StatusCode: 529}), "retryable(500 error) = false, want true")
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
	err := &anthropic.Error{StatusCode: 400}
	c.Require().NoError(err.UnmarshalJSON([]byte(creditBody)), "UnmarshalJSON")
	c.False(retryable(err), "retryable(credit-exhausted) = true, want false (retrying it is pointless)")
	c.True(FailoverWorthy(err), "FailoverWorthy(credit-exhausted) = false, want true")
	// An ordinary 400 stays non-failover-worthy: failing over would just
	// re-send a malformed request to a second provider.
	bad := &anthropic.Error{StatusCode: 400}
	c.Require().NoError(bad.UnmarshalJSON([]byte(`{"error":{"message":"max_tokens: must be >= 1"}}`)), "UnmarshalJSON")
	c.False(FailoverWorthy(bad), "FailoverWorthy(ordinary 400) = true, want false")
	// Nil and status-only errors must not panic (RawJSON, not Error(), which
	// dereferences Request/Response).
	c.False(FailoverWorthy(nil), "FailoverWorthy(nil) = true, want false")
	c.True(FailoverWorthy(&anthropic.Error{StatusCode: 529}), "FailoverWorthy(529) = false, want true")
}
