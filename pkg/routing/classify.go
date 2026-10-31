// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
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
// definition shared by the native classifier (retryable, via a *msg.APIError's
// embedded status) and the HTTP proxy path (ServeHTTP, via resp.StatusCode +
// err).
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

// creditExhausted is the native counterpart of CreditExhausted, reading the
// rejection body off a *msg.APIError. An in-band error (Status 0) carries no
// status to restrict the match to, so it is not a billing rejection.
func creditExhausted(err error) bool {
	var apiErr *msg.APIError
	if !errors.As(err, &apiErr) || apiErr.Status == 0 {
		return false
	}
	return CreditExhausted(apiErr.Status, apiErr.Body)
}

// FailoverWorthy reports whether an error means the primary should be abandoned
// for the fallback chain (and the breaker tripped): a retryable failure, or a
// credit-exhausted account. It is the one failover gate — retryable alone would
// strand every caller on a primary that cannot answer until someone pays the
// bill.
func FailoverWorthy(err error) bool { return retryable(err) || creditExhausted(err) }

// retryable classifies a provider error, mapping its embedded HTTP status onto
// ClassifyFailure. A *msg.APIError with Status 0 is an in-band stream error:
// it is retryable when its kind says the upstream had a bad moment, and not
// otherwise, because a status-less error has no status to classify.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *msg.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status != 0 {
			return ClassifyFailure(apiErr.Status, nil)
		}
		return apiErr.Kind == msg.ErrTransient || apiErr.Kind == msg.ErrOverloaded
	}
	return ClassifyFailure(0, err) // transport/timeout or a status-less failure
}
