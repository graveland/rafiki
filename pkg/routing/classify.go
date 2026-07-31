// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// RetryBackoffs is the fixed backoff schedule between primary retry attempts
// before failing over to OpenRouter. 3 retries (4 total attempts against the
// primary) — most Anthropic 5xx/429s are transient blips, not outages, so a
// short retry burst avoids failing over (and losing prompt-cache locality) on
// noise. Fixed, not exponential-with-jitter: the schedule is short-lived and
// per-request, no thundering-herd risk to guard against.
var RetryBackoffs = []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second}

// ClassifyFailure reports whether a primary attempt is a retryable upstream
// failure worth failing over on: a 5xx or 429 status, or a transport error with
// no HTTP status — but NOT client cancellation (context.Canceled /
// DeadlineExceeded), which is not an upstream-health signal. It is the single
// definition shared by the SDK path (Core.callModel, via the error's embedded
// status) and the HTTP proxy path (ServeHTTP, via resp.StatusCode + err).
//
// statusCode is the HTTP status of a completed transport call (0 when the call
// itself errored before yielding a response); err is that call's error, if any.
func ClassifyFailure(statusCode int, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true // transport/timeout errors (no HTTP status) are retryable
	}
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

// creditMarkers identify an account-level billing rejection in a provider's
// error body. Matched case-insensitively as substrings, because providers
// return these as prose inside error.message with no machine-readable code
// distinguishing them from any other invalid_request_error:
//
//	{"type":"error","error":{"type":"invalid_request_error","message":
//	 "Your credit balance is too low to access the Anthropic API. ..."}}
var creditMarkers = []string{
	"credit balance is too low", // Anthropic (400 invalid_request_error)
	"insufficient credits",      // OpenRouter (402)
	"insufficient_quota",        // OpenAI-shaped upstreams
}

// CreditExhausted reports whether an upstream rejection is an account-level
// billing failure — the account is out of credit, so the SAME request will
// fail identically until a human tops it up.
//
// This is deliberately NOT folded into ClassifyFailure: retrying the primary
// is pointless here (unlike a 5xx blip), but the primary IS unusable, so
// failing over to the fallback and tripping the breaker is exactly right.
// Callers must therefore treat it as "fail over now, without retrying".
//
// Restricted to the statuses providers actually use for billing rejections,
// so an unrelated 4xx whose body happens to quote one of these phrases (an
// echoed prompt, say) can't masquerade as one.
func CreditExhausted(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden:
	default:
		return false
	}
	lower := bytes.ToLower(body)
	for _, m := range creditMarkers {
		if bytes.Contains(lower, []byte(m)) {
			return true
		}
	}
	return false
}

// creditExhausted is the SDK-error counterpart of CreditExhausted, reading the
// rejection body off an *anthropic.Error. RawJSON (not Error(), which
// dereferences Request/Response) so a status-only error value is safe.
func creditExhausted(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return CreditExhausted(apiErr.StatusCode, []byte(apiErr.RawJSON()))
}

// FailoverWorthy reports whether an SDK error means the primary should be
// abandoned for the fallback chain (and the breaker tripped): a retryable
// failure, or a credit-exhausted account. It is the SDK path's single
// failover gate — Retryable alone would strand every caller on a primary that
// cannot answer until someone pays the bill.
func FailoverWorthy(err error) bool { return retryable(err) || creditExhausted(err) }

// Retryable classifies an Anthropic SDK error, mapping its embedded HTTP
// status onto ClassifyFailure — the SDK-error counterpart used by the llm
// package's per-upstream routing.
func Retryable(err error) bool { return retryable(err) }

// retryable classifies an Anthropic SDK error, mapping its embedded HTTP status
// onto ClassifyFailure. An *anthropic.Error with StatusCode 0 (SDK-side error
// before a response) is treated like a transport error and is retryable.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
		return ClassifyFailure(apiErr.StatusCode, nil)
	}
	return ClassifyFailure(0, err) // transport/timeout or status-less SDK error
}
