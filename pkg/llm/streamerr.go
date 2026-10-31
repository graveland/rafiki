// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// An in-band "error" event — the shape BOTH api.anthropic.com and OpenRouter's
// compatible face send when an upstream fails after the connection is already
// up — reaches this package as a *msg.APIError with Status 0: there was no HTTP
// status, because the connection had already come up, and the event's own JSON
// is the body.
//
// The codec that decoded the stream classified the event (msg.ErrorKind), which
// is what the retry decisions read. The parsers below exist for the two things
// a kind cannot say: the payload's own error.type, and the numeric code some
// upstreams put in it.

// StreamError is the parsed payload of an SSE "error" event.
type StreamError struct {
	Type    string // outer envelope type ("error"), or the inner type on a bare payload
	ErrType string // error.type: timeout_error / overloaded_error / api_error / invalid_request_error / ...
	Message string // error.message: human-readable detail
	Code    int    // numeric code when present (0 when absent): 502/504/529/...
}

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

// ParseStreamError extracts the payload of an in-band SSE "error" event. ok is
// false for every other error shape — a status-carrying APIError, a transport
// error, nil.
//
// Two envelopes parse: the protocol's double envelope
// ({"type":"error","error":{...}}) and the bare inner object
// ({"type":"timeout_error",...}) some providers emit.
func ParseStreamError(err error) (StreamError, bool) {
	apiErr, ok := inBandAPIError(err)
	if !ok {
		return StreamError{}, false
	}
	se, _ := parseInBandError(apiErr.Body)
	return StreamError{Type: se.Type, ErrType: se.ErrType, Message: se.Message, Code: se.Code}, true
}

// parseInBandError reads either envelope an in-band error event uses. The bool
// is false when the body is not JSON at all, which the callers treat as "the
// event named nothing".
func parseInBandError(data []byte) (StreamError, bool) {
	var wire struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   *struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return StreamError{}, false
	}
	if wire.Error == nil {
		return StreamError{Type: wire.Type, ErrType: wire.Type, Message: wire.Message}, true
	}
	se := StreamError{Type: wire.Type, ErrType: wire.Error.Type, Message: wire.Error.Message}
	if len(wire.Error.Code) > 0 && string(wire.Error.Code) != "null" {
		if n, err := strconv.Atoi(strings.Trim(string(wire.Error.Code), `"`)); err == nil {
			se.Code = n
		}
	}
	return se, true
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
