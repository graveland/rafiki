// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"errors"

	"go.graveland.dev/rafiki/pkg/msg"
)

// An in-band "error" event — the shape BOTH api.anthropic.com and OpenRouter's
// compatible face send when an upstream fails after the connection is already
// up — reaches this package as a *msg.APIError with Status 0: there was no HTTP
// status, because the connection had already come up, and the event's own JSON
// is the body.
//
// The codec that decoded the stream classified the event (msg.ErrorKind), and
// that classification is all the retry decisions here read; the event's payload
// is parsed once, by the codec that owns its dialect.

// inBandAPIError returns err as the in-band API error it is, or false for
// anything else: a status-carrying refusal is an ordinary HTTP error, not a
// stream event.
func inBandAPIError(err error) (*msg.APIError, bool) {
	var apiErr *msg.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Status != 0 {
		return nil, false
	}
	return apiErr, true
}

// IsTransientStreamError reports whether err is an in-band SSE "error" event
// describing a TRANSIENT upstream failure: the kind the codec classified as
// ErrTransient or ErrOverloaded. The intended consumer is a retry loop above
// this package (agentloop's continueWithRetry): the failed attempt persisted
// nothing to the conversation — history writes happen only on a completed
// stream — so re-issuing Continue is safe. A mid-stream failure can have
// delivered partial content to a stream handler; that concern belongs to the
// caller, whose own delivery guard decides whether a retry is safe at all.
//
// Rate limits are transient too but are NOT reported here: they belong on
// IsRateLimitStreamError, so each rejection takes exactly one retry path —
// sendStreaming's ModelGate loop, never agentloop's generic backoff stacked on
// top of it.
func IsTransientStreamError(err error) bool {
	apiErr, ok := inBandAPIError(err)
	if !ok {
		return false
	}
	return apiErr.Kind == msg.ErrTransient || apiErr.Kind == msg.ErrOverloaded
}

// IsRateLimitStreamError reports whether err is an in-band SSE "error" event
// carrying a rate-limit rejection — the same "no more budget upstream" answer a
// typed 429 carries, arriving after connection setup instead of as a response
// status. There is no Retry-After header recoverable from an in-band payload, so
// consumers fall back to exponential backoff.
func IsRateLimitStreamError(err error) bool {
	apiErr, ok := inBandAPIError(err)
	return ok && apiErr.Kind == msg.ErrRateLimit
}
