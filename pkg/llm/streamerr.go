// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// An in-band Anthropic-protocol "error" event — the shape BOTH api.anthropic.com
// and OpenRouter's compatible face send when an upstream fails after the
// connection is already up — never becomes a typed *anthropic.Error. The
// SDK's ssestream (v1.37.0, Stream.Next's "error" case) wraps the raw event
// body in a plain fmt.Errorf and nothing else survives:
//
//	fmt.Errorf("received error while streaming: %s", rawEventData)
//
// Concretely, OpenRouter models abort mid-turn with exactly this:
//
//	received error while streaming: {"type":"error","error":{"type":"timeout_error","message":"The operation was aborted","error_type":"timeout"}}
//
// Every classifier keyed on typed errors — *anthropic.Error status codes,
// net.OpError, syscalls — sees a plain string. The only machine-readable
// signal left is inside the wrapped JSON itself, so the parsers below exist
// to recover it.
const sseStreamErrPrefix = "received error while streaming: "

// StreamError is the parsed payload of an SSE "error" event.
type StreamError struct {
	Type    string // outer envelope type ("error"), or the inner type on a bare payload
	ErrType string // error.type: timeout_error / overloaded_error / api_error / invalid_request_error / ...
	Message string // error.message: human-readable detail
	Code    int    // numeric code when present (0 when absent): 502/504/529/...
}

// transientStreamErrorTypes are the error.type values that mean "the upstream
// had a bad moment; re-issuing may well succeed" — Anthropic's own
// 500-class names plus OpenRouter's in-band timeout shape. Everything else
// (invalid_request_error, authentication_error, permission_error,
// not_found_error, request_too_large, billing...) describes OUR request, not
// the upstream's health, so it must fail fast rather than burn backoff
// retries against an answer that will be identical every time.
//
// rate_limit_error is deliberately NOT here even though it is transient:
// a rate limit needs backPRESSURE, not generic per-turn backoff. It is
// classified separately below so consumers can route it to the ModelGate.
var transientStreamErrorTypes = map[string]bool{
	"timeout_error":    true,
	"overloaded_error": true,
	"api_error":        true,
}

// rateLimitStreamErrorTypes are the error.type values that mean "HTTP 429
// arrived after the connection was already up". Anthropic's protocol spells
// this rate_limit_error; OpenRouter relays upstream failures loosely, so its
// native spelling rate_limit_exceeded is accepted too — the live payload that
// motivated this carried both:
//
//	received error while streaming: {"type":"error","error":{"type":
//	"rate_limit_error","message":"Provider returned error","error_type":
//	"rate_limit_exceeded"}}
//
// Like IsTransientStreamError this is consumed by sendStreaming's rate-limit
// loop: record429 blocks every send for that model — across ALL
// conversations — while backing off, which is what a shared provider limit
// actually wants. The two classifiers stay disjoint on purpose; the split
// mirrors agentloop.isRetryable's handling of TYPED errors (429 → "already
// handled by sendStreaming", everything else 5xx → its generic retry loop).
var rateLimitStreamErrorTypes = map[string]bool{
	"rate_limit_error":    true,
	"rate_limit_exceeded": true,
}

// ParseStreamError extracts the SSE error payload carried by the opaque
// wrapper the SDK produces for a stream-level "error" event (see
// sseStreamErrPrefix). ok is false for every other error shape — typed
// *anthropic.Error values, net errors, nil.
//
// Two envelopes parse: the protocol's double envelope shown above, and the
// bare inner object ({"type":"timeout_error",...}) some providers emit.
func ParseStreamError(err error) (StreamError, bool) {
	if err == nil {
		return StreamError{}, false
	}
	// A typed *anthropic.Error is classified by status code elsewhere; it is
	// also unsafe to stringify (Error() dereferences Request/Response, so a
	// status-only value panics — the same caveat routing/classify.go records
	// for RawJSON). The ssestream wrapper never wraps one, so exclude it here.
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return StreamError{}, false
	}
	i := strings.Index(err.Error(), sseStreamErrPrefix)
	if i < 0 {
		return StreamError{}, false
	}
	payload := err.Error()[i+len(sseStreamErrPrefix):]

	var wire struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   *struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(payload), &wire); jsonErr != nil {
		return StreamError{}, false
	}
	se := StreamError{Type: wire.Type}
	if wire.Error == nil {
		// Bare envelope: type and message sit at the top level.
		se.ErrType = wire.Type
		se.Message = wire.Message
		return se, true
	}
	se.ErrType = wire.Error.Type
	se.Message = wire.Error.Message
	if len(wire.Error.Code) > 0 && string(wire.Error.Code) != "null" {
		if n, cErr := strconv.Atoi(strings.Trim(string(wire.Error.Code), `"`)); cErr == nil {
			se.Code = n
		}
	}
	return se, true
}

// IsTransientStreamError reports whether err is an in-band SSE "error" event
// whose payload describes a TRANSIENT upstream failure: a known-transient
// error.type, or any numeric code >= 500. The intended consumer is a retry
// loop above this package (agentloop's continueWithRetry): the failed attempt
// persisted nothing to the conversation — history writes happen only on a
// completed stream — so re-issuing Continue is safe. A mid-stream failure can
// have delivered partial content to a stream handler; that concern belongs to
// the caller, whose own delivery guard decides whether a retry is safe at all.
//
// Rate limits are transient too but are NOT reported here: they belong on
// IsRateLimitStreamError, so each rejection takes exactly one retry path —
// sendStreaming's ModelGate loop, never agentloop's generic backoff stacked
// on top of it.
func IsTransientStreamError(err error) bool {
	se, ok := ParseStreamError(err)
	if !ok {
		return false
	}
	if se.Code >= 500 {
		return true
	}
	return transientStreamErrorTypes[se.ErrType]
}

// IsRateLimitStreamError reports whether err is an in-band SSE "error" event
// carrying a rate-limit rejection — the same "no more budget upstream"
// answer a typed 429 carries, arriving after connection setup instead of as
// a response status. There is no Retry-After header recoverable from an
// in-band payload, so consumers fall back to exponential backoff.
func IsRateLimitStreamError(err error) bool {
	se, ok := ParseStreamError(err)
	return ok && rateLimitStreamErrorTypes[se.ErrType]
}
