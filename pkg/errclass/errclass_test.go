// SPDX-License-Identifier: Apache-2.0

package errclass_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/errclass"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/responses"
)

const (
	creditBody  = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`
	authBody    = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	permBody    = `{"type":"error","error":{"type":"permission_error","message":"your key cannot use this model"}}`
	openAIQuota = `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`
	promptBody  = `{"error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`
	orNoEnd     = `{"error":{"message":"No endpoints found that can handle the requested parameters","code":404}}`
	overloaded  = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	badBody     = `{"type":"error","error":{"type":"invalid_request_error","message":"messages: text content blocks must be non-empty"}}`
)

func TestFromHTTPClassifiesEveryRealRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   errclass.Class
	}{
		{"credit exhausted is a 400 and must not read as a bad request", 400, creditBody, errclass.UsageLimit},
		{"openai quota arrives as a 429 and must not read as a rate limit", 429, openAIQuota, errclass.UsageLimit},
		{"invalid key", 401, authBody, errclass.Auth},
		{"forbidden with no body", 403, "", errclass.Auth},
		{"permission error body on a 400", 400, permBody, errclass.Auth},
		{"plain rate limit", 429, `{"error":{"type":"rate_limit_error"}}`, errclass.RateLimit},
		{"overloaded status", 529, "", errclass.Overloaded},
		{"overloaded body on a 200-range-looking status", 503, overloaded, errclass.Overloaded},
		{"server error", 500, "boom", errclass.Transient},
		{"bad gateway", 502, "", errclass.Transient},
		{"prompt too long", 400, promptBody, errclass.PromptTooLarge},
		{"openrouter no endpoints", 404, orNoEnd, errclass.NoEligible},
		{"ordinary bad request", 400, badBody, errclass.BadRequest},
		{"not found", 404, "", errclass.BadRequest},
		{"a status that is not an error", 200, "", errclass.Other},
		{"redirect", 302, "", errclass.Other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewAborting(t).Eq(tc.want, errclass.FromHTTP(tc.status, []byte(tc.body)))
		})
	}
}

type statusErr struct {
	status int
	body   string
}

func (e *statusErr) Error() string             { return "status error" }
func (e *statusErr) HTTPStatus() (int, []byte) { return e.status, []byte(e.body) }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type resetErr struct{}

func (resetErr) Error() string   { return "connection reset" }
func (resetErr) Timeout() bool   { return false }
func (resetErr) Temporary() bool { return false }

func TestFromErrorClassifiesTypedErrorsWithoutChangingTheirKind(t *testing.T) {
	// The credit-exhausted refusal is ErrOther by Kind. The class must see
	// through it, and the Kind must be left alone: failover keys on it.
	credit := &msg.APIError{Status: http.StatusBadRequest, Body: []byte(creditBody), Kind: msg.ErrOther, Dialect: msg.DialectAnthropic}
	auth := &msg.APIError{Status: http.StatusUnauthorized, Body: []byte(authBody), Kind: msg.ErrOther, Dialect: msg.DialectAnthropic}
	rl := &msg.APIError{Status: 429, Kind: msg.ErrRateLimit}
	kindOnly := &msg.APIError{Status: 400, Body: []byte(badBody), Kind: msg.ErrPromptTooLarge}
	inBand := &msg.APIError{Status: 0, Body: []byte(`{"type":"error"}`), Kind: msg.ErrOther}
	inBandOverloaded := &msg.APIError{Status: 0, Kind: msg.ErrOverloaded}

	for _, tc := range []struct {
		name string
		err  error
		want errclass.Class
	}{
		{"nil is no class", nil, ""},
		{"credit exhausted", credit, errclass.UsageLimit},
		{"wrapped credit exhausted", fmt.Errorf("llm: send: %w", credit), errclass.UsageLimit},
		{"revoked key", auth, errclass.Auth},
		{"rate limit by kind", rl, errclass.RateLimit},
		{"kind outranks a body that says bad request", kindOnly, errclass.PromptTooLarge},
		{"in-band error with nothing else is a stream error", inBand, errclass.StreamError},
		{"in-band overloaded keeps its kind", inBandOverloaded, errclass.Overloaded},
		{"responses usage limit is not an APIError", &responses.UsageLimitError{Code: "insufficient_quota"}, errclass.UsageLimit},
		{"wrapped responses usage limit", fmt.Errorf("send: %w", &responses.UsageLimitError{Code: "usage_limit_reached"}), errclass.UsageLimit},
		{"embedding client http error", &statusErr{status: 429, body: openAIQuota}, errclass.UsageLimit},
		{"embedding client auth error", &statusErr{status: 401}, errclass.Auth},
		{"caller went away", context.Canceled, errclass.ClientCanceled},
		{"wrapped cancel", fmt.Errorf("turn: %w", context.Canceled), errclass.ClientCanceled},
		{"deadline", context.DeadlineExceeded, errclass.Timeout},
		{"network timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, errclass.Timeout},
		{"network failure", &net.OpError{Op: "read", Err: resetErr{}}, errclass.Network},
		{"anything else", errors.New("something odd"), errclass.Other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewAborting(t).Eq(tc.want, errclass.FromError(tc.err))
		})
	}

	assert.NewAborting(t).Eq(msg.ErrOther, credit.Kind, "classifying must not rewrite the Kind that failover keys on")
}

func TestEveryClassIsListedOnceWithALabelSafeSpelling(t *testing.T) {
	c := assert.NewAborting(t)
	seen := map[errclass.Class]bool{}
	for _, cl := range errclass.All() {
		c.False(seen[cl], "duplicate class %q", cl)
		seen[cl] = true
		c.NotEq("", string(cl), "empty class")
		for _, r := range string(cl) {
			c.True(r == '_' || (r >= 'a' && r <= 'z'), "class %q is not a snake_case label value", cl)
		}
	}
	c.Eq(14, len(seen), "the vocabulary is fixed; adding a class is a documented change")
}
