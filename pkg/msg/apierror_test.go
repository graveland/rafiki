package msg_test

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/msg"
)

// TestAPIErrorMessage pins the rendered form: the parsed provider message wins
// over the body, and an unparsed body is truncated to 300 bytes.
func TestAPIErrorMessage(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		kind msg.ErrorKind
		want string
	}{
		{kind: msg.ErrOther, want: "other"},
		{kind: msg.ErrRateLimit, want: "rate_limit"},
		{kind: msg.ErrPromptTooLarge, want: "prompt_too_large"},
		{kind: msg.ErrOverloaded, want: "overloaded"},
		{kind: msg.ErrNoEligible, want: "no_eligible"},
		{kind: msg.ErrUsageLimit, want: "usage_limit"},
		{kind: msg.ErrTransient, want: "transient"},
	} {
		c.Eq(tc.want, tc.kind.String(), "ErrorKind spelling")
	}

	parsed := &msg.APIError{
		Status:  429,
		Kind:    msg.ErrRateLimit,
		Dialect: msg.DialectAnthropic,
		Message: "slow down",
		Body:    []byte(`{"error":{"message":"slow down"}}`),
	}
	c.Eq("llm: anthropic api error: status 429 (rate_limit): slow down", parsed.Error(),
		"a parsed provider message wins over the body")

	body := strings.Repeat("x", 400)
	truncated := &msg.APIError{
		Status:  500,
		Kind:    msg.ErrOverloaded,
		Dialect: msg.DialectOpenAIChat,
		Body:    []byte(body),
	}
	want := "llm: openai-chat api error: status 500 (overloaded): " + body[:300]
	c.Eq(want, truncated.Error(), "an unparsed body is truncated to its first 300 bytes")
	c.Eq(len("llm: openai-chat api error: status 500 (overloaded): ")+300, len(truncated.Error()))
}
