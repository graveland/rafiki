// SPDX-License-Identifier: Apache-2.0

// Package errclass names why an LLM or embedding call failed, as a small fixed
// vocabulary suitable for a metric label.
//
// It is a read-only view over an error or an HTTP refusal. It deliberately does
// not feed back into msg.ErrorKind: that kind drives retry and failover, and
// the account being out of credit (an Anthropic 400 classified ErrOther) is
// failover-worthy on purpose. Changing the kind to make a metric nicer would
// change what the daemon does about the failure.
package errclass

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// Class is one failure category. Its spelling is a metric label value, so the
// set is fixed and small: adding a class is a deliberate, documented change.
type Class string

const (
	// UsageLimit: the account is out of credit or quota. Nothing succeeds
	// until someone tops it up.
	UsageLimit Class = "usage_limit"
	// Auth: the credential was refused (revoked, wrong, or lacking permission).
	Auth Class = "auth"
	// RateLimit: the provider asked us to back off.
	RateLimit Class = "rate_limit"
	// Overloaded: the provider is saturated.
	Overloaded Class = "overloaded"
	// Transient: a 5xx the provider is expected to recover from.
	Transient Class = "transient"
	// Timeout: a deadline ended the call.
	Timeout Class = "timeout"
	// Network: the connection failed before or during the exchange.
	Network Class = "network"
	// PromptTooLarge: the request outgrew the model's context.
	PromptTooLarge Class = "prompt_too_large"
	// NoEligible: routing found no provider that can serve the request.
	NoEligible Class = "no_eligible"
	// BadRequest: the provider rejected the request itself (a 4xx that is not
	// one of the classes above).
	BadRequest Class = "bad_request"
	// ClientCanceled: the caller went away; the provider is not implicated.
	ClientCanceled Class = "client_canceled"
	// StreamError: the response stream broke after it started.
	StreamError Class = "stream_error"
	// CaptureParse: the response arrived but the daemon could not parse it for
	// capture; the provider is not at fault.
	CaptureParse Class = "capture_parse"
	// Other: nothing above applies.
	Other Class = "other"
)

// All lists every class, in documentation order.
func All() []Class {
	return []Class{UsageLimit, Auth, RateLimit, Overloaded, Transient, Timeout, Network,
		PromptTooLarge, NoEligible, BadRequest, ClientCanceled, StreamError, CaptureParse, Other}
}

// usageMarkers are the spellings providers use when the account, rather than
// the request, is out of budget. Matched lower-cased against the body.
var usageMarkers = []string{
	"credit balance is too low",
	"insufficient_quota",
	"usage_limit_reached",
	"usage_not_included",
	"exceeded your current quota",
	// OpenRouter: 402 Payment Required.
	"insufficient credits",
	"requires more credits",
	"billing_error",
	"payment_required",
}

// authMarkers are the provider error types and messages for a refused
// credential.
var authMarkers = []string{
	"authentication_error",
	"permission_error",
	"invalid x-api-key",
	"invalid api key",
	"incorrect api key",
}

func hasAny(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// FromHTTP classifies a provider's HTTP refusal from its status and body. A
// body naming an exhausted account wins over the status, because those arrive
// as 400 and 429 and look like a bad request or a rate limit.
func FromHTTP(status int, body []byte) Class {
	lower := strings.ToLower(string(body))
	switch {
	case status == http.StatusPaymentRequired || hasAny(lower, usageMarkers):
		return UsageLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden || hasAny(lower, authMarkers):
		return Auth
	}
	switch anthropic.Classify(status, nil, body) {
	case msg.ErrRateLimit:
		return RateLimit
	case msg.ErrOverloaded:
		return Overloaded
	case msg.ErrPromptTooLarge:
		return PromptTooLarge
	case msg.ErrNoEligible:
		return NoEligible
	case msg.ErrTransient:
		return Transient
	case msg.ErrUsageLimit:
		return UsageLimit
	}
	if status >= 400 && status < 500 {
		return BadRequest
	}
	return Other
}

// statuser is how an error exposes the raw refusal it carries: *msg.APIError
// and the embedding client's HTTP error both do.
type statuser interface {
	HTTPStatus() (int, []byte)
}

// FromError classifies a failed call. A nil error is the empty class.
func FromError(err error) Class {
	if err == nil {
		return ""
	}
	var usage *responses.UsageLimitError
	if errors.As(err, &usage) {
		return UsageLimit
	}
	if errors.Is(err, context.Canceled) {
		return ClientCanceled
	}
	var api *msg.APIError
	if errors.As(err, &api) {
		return fromAPIError(api)
	}
	var st statuser
	if errors.As(err, &st) {
		status, body := st.HTTPStatus()
		return FromHTTP(status, body)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return Timeout
		}
		return Network
	}
	return Other
}

func fromAPIError(e *msg.APIError) Class {
	// The raw refusal decides the account-level classes first: credit
	// exhaustion and a refused key are both ErrOther by Kind.
	if c := FromHTTP(e.Status, e.Body); c == UsageLimit || c == Auth {
		return c
	}
	switch e.Kind {
	case msg.ErrUsageLimit:
		return UsageLimit
	case msg.ErrRateLimit:
		return RateLimit
	case msg.ErrOverloaded:
		return Overloaded
	case msg.ErrTransient:
		return Transient
	case msg.ErrPromptTooLarge:
		return PromptTooLarge
	case msg.ErrNoEligible:
		return NoEligible
	}
	if e.Status == 0 {
		// An in-band stream error carries no status: Kind was all there was.
		return StreamError
	}
	return FromHTTP(e.Status, e.Body)
}
