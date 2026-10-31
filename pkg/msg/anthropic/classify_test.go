package anthropic_test

import (
	"net/http"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// TestClassify pins every classification rule a sender branches on: one row
// per rule, plus the fallthrough that must stay non-retryable.
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   msg.ErrorKind
	}{
		{"rate limit", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error"}}`, msg.ErrRateLimit},
		{"prompt too large", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens"}}`, msg.ErrPromptTooLarge},
		{"no endpoints found", http.StatusNotFound, `{"error":{"message":"No endpoints found for anthropic/claude"}}`, msg.ErrNoEligible},
		{"no allowed providers", http.StatusBadRequest, `{"error":{"message":"No allowed providers are available"}}`, msg.ErrNoEligible},
		{"overloaded status", 529, `{}`, msg.ErrOverloaded},
		{"overloaded body", http.StatusBadRequest, `{"error":{"type":"overloaded_error","message":"Overloaded"}}`, msg.ErrOverloaded},
		{"server error", http.StatusInternalServerError, `{}`, msg.ErrTransient},
		{"gateway timeout", http.StatusGatewayTimeout, `{}`, msg.ErrTransient},
		{"plain 400", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"bad request"}}`, msg.ErrOther},
		{"plain 404", http.StatusNotFound, `{}`, msg.ErrOther},
		{"unauthorized", http.StatusUnauthorized, `{}`, msg.ErrOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			c.Eq(tc.want, anthropic.Classify(tc.status, http.Header{"Retry-After": []string{"5"}}, []byte(tc.body)))
		})
	}
}
