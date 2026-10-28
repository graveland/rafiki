// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// UsageLimitError is a subscription usage limit: terminal for the turn, never
// retried, because the reset is hours or days away and retries only burn
// budget.
//
// It is deliberately NOT an *anthropic.Error. Two classifiers above this
// package would otherwise misread it: pkg/llm's isRateLimit retries any 429
// *anthropic.Error through the ModelGate's backoff (burning the whole retry
// budget against a limit that resets tomorrow), and pkg/agentloop's
// isRetryable has string fallbacks that retry anything whose text looks like a
// transport failure. A distinct type makes "the account is out of quota"
// impossible to confuse with "try again in a moment", and Error() deliberately
// avoids every fragment those fallbacks look for.
type UsageLimitError struct {
	Code     string    // usage_limit_reached, usage_not_included, insufficient_quota
	PlanType string    // the account's plan as the endpoint reports it; "" when unknown
	ResetsAt time.Time // zero = unknown
}

// usageLimitCodes are the error codes (or types) the ChatGPT backend answers
// with when the account, rather than the request, is out of budget.
var usageLimitCodes = map[string]bool{
	"usage_limit_reached": true,
	"usage_not_included":  true,
	"insufficient_quota":  true,
}

// Error renders the limit in the terms the operator can act on: which code, the
// plan, and when it lifts.
func (e *UsageLimitError) Error() string {
	plan := e.PlanType
	if plan == "" {
		plan = "unknown"
	}
	if e.ResetsAt.IsZero() {
		return fmt.Sprintf("ChatGPT usage limit reached (%s, plan %s); reset time unknown", e.Code, plan)
	}
	rest := time.Until(e.ResetsAt).Round(time.Minute)
	return fmt.Sprintf("ChatGPT usage limit reached (%s, plan %s); resets at %s (in %s)",
		e.Code, plan, e.ResetsAt.UTC().Format(time.RFC3339), rest)
}

// responsesAPIError is the error object the Responses endpoint puts either at
// the top level of an SSE "error" event or under "error" in an HTTP body.
type responsesAPIError struct {
	Code     json.RawMessage `json:"code"`
	Type     string          `json:"type"`
	Message  string          `json:"message"`
	PlanType string          `json:"plan_type"`
	ResetsAt *int64          `json:"resets_at"`
}

// responsesAPIErrorEnvelope accepts both spellings the endpoint uses: the
// nested {"error":{...}} of an HTTP body (and of some SSE error events) and the
// flat {"type":...,"code":...,"message":...} of others.
type responsesAPIErrorEnvelope struct {
	Type    string             `json:"type"`
	Code    json.RawMessage    `json:"code"`
	Message string             `json:"message"`
	Error   *responsesAPIError `json:"error"`
}

// decodeResponsesAPIError parses an error payload leniently: a body that is not
// the expected JSON at all yields a zero envelope rather than an error, because
// the caller's fallback (an ordinary status-carrying error) is always correct.
func decodeResponsesAPIError(body []byte) responsesAPIErrorEnvelope {
	var env responsesAPIErrorEnvelope
	if len(body) == 0 {
		return env
	}
	_ = json.Unmarshal(body, &env)
	return env
}

// info flattens the envelope into the fields the classifiers read, preferring
// the nested object and falling back to the flat one.
func (env responsesAPIErrorEnvelope) info() (code, planType, message string, resetsAt *int64) {
	if e := env.Error; e != nil {
		code = rawJSONCode(e.Code)
		if code == "" {
			code = e.Type
		}
		planType, message, resetsAt = e.PlanType, e.Message, e.ResetsAt
	}
	if code == "" {
		code = rawJSONCode(env.Code)
	}
	if code == "" {
		code = env.Type
	}
	if message == "" {
		message = env.Message
	}
	return code, planType, message, resetsAt
}

// rawJSONCode renders a "code" field that is sometimes a string, sometimes a
// number, and sometimes absent.
func rawJSONCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

// isUsageLimitCode reports whether code names a subscription usage limit.
func isUsageLimitCode(code string) bool { return usageLimitCodes[code] }

// responsesUsageLimitError builds the terminal error for an HTTP response, or
// nil when the response is not a usage limit.
//
// Only a 429 and a 404 can carry it: the ChatGPT backend answers a spent
// subscription with 429 normally, and some clients see the same body on a 404
// (the shape occa works around). Any other status stays an ordinary retryable
// or terminal error, and a 429 without a usage-limit code stays a retryable
// 429.
func responsesUsageLimitError(status int, header http.Header, body []byte) *UsageLimitError {
	if status != http.StatusTooManyRequests && status != http.StatusNotFound {
		return nil
	}
	env := decodeResponsesAPIError(body)
	code, planType, _, resetsAt := env.info()
	if !isUsageLimitCode(code) {
		return nil
	}
	if planType == "" {
		planType = header.Get("x-codex-plan-type")
	}
	e := &UsageLimitError{Code: code, PlanType: planType}
	if resetsAt != nil {
		e.ResetsAt = time.Unix(*resetsAt, 0)
	} else if t := responsesResetTime(header, time.Now()); !t.IsZero() {
		e.ResetsAt = t
	}
	return e
}

// responsesUsageLimitFromEvent builds the same error from an in-stream error
// payload, where no HTTP headers exist to fall back on.
func responsesUsageLimitFromEvent(env responsesAPIErrorEnvelope) *UsageLimitError {
	code, planType, _, resetsAt := env.info()
	if !isUsageLimitCode(code) {
		return nil
	}
	e := &UsageLimitError{Code: code, PlanType: planType}
	if resetsAt != nil {
		e.ResetsAt = time.Unix(*resetsAt, 0)
	}
	return e
}

// responsesResetTime reads the reset instant from the rate-limit headers the
// ChatGPT backend sends alongside every response. The header FAMILY depends on
// which window is currently binding: x-codex-active-limit says "codex_other"
// when the other (secondary) window is the one in force, and that family's
// headers are the ones carrying its reset. Hardcoding the primary pair (as
// most third-party clients do) reports the wrong reset time whenever the other
// window is the one that ran out.
func responsesResetTime(header http.Header, now time.Time) time.Time {
	if header == nil {
		return time.Time{}
	}
	family := "primary"
	if header.Get("x-codex-active-limit") == "codex_other" {
		family = "other-primary"
	}
	v := header.Get("x-codex-" + family + "-reset-after-seconds")
	if v == "" {
		return time.Time{}
	}
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs) * time.Second)
}

// promptTooLargePhrase is the substring pkg/llm/trim.go's isPromptTooLarge
// matches on. The predicate is defined over an *anthropic.Error carrying a 400
// and this phrase, which is how the Anthropic-shaped path learns to trim; the
// Responses endpoint reports the same condition in-band as the code
// context_length_exceeded, so the sender reconstitutes the shape the predicate
// understands rather than adding a second predicate (trim.go owns the rule).
const promptTooLargePhrase = "prompt is too long"

// newResponsesPromptTooLargeError builds the trim-triggering error for a
// context overflow reported inside the stream. detail is the endpoint's own
// message and rides along after the phrase so the log still names what
// happened.
//
// The request and response are synthetic: an in-band event has neither, and
// *anthropic.Error's Error() dereferences both, so a nil either would panic the
// first time anything logs it.
func newResponsesPromptTooLargeError(detail string) error {
	req, err := http.NewRequest(http.MethodPost, chatGPTBaseURL+"/responses", nil)
	if err != nil {
		// Unreachable: a constant URL with no body cannot fail to parse.
		return fmt.Errorf("llm: openai-responses: context length exceeded: %s", detail)
	}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{},
	}
	msg := promptTooLargePhrase
	if detail != "" && !strings.Contains(strings.ToLower(detail), promptTooLargePhrase) {
		msg = promptTooLargePhrase + ": " + detail
	} else if detail != "" {
		msg = detail
	}
	body, mErr := json.Marshal(map[string]any{"error": map[string]any{
		"type":    "invalid_request_error",
		"message": msg,
	}})
	if mErr != nil {
		return fmt.Errorf("llm: openai-responses: context length exceeded: %s", detail)
	}
	apiErr := &anthropic.Error{Request: req, Response: resp}
	// UnmarshalJSON is the only way to populate Error's unexported raw body;
	// setting StatusCode afterwards keeps it independent of the payload.
	_ = json.Unmarshal(body, apiErr)
	apiErr.StatusCode = http.StatusBadRequest
	return apiErr
}
