package msg

import (
	"fmt"
	"net/http"
	"time"
)

// ErrorKind classifies a provider API error into what a sender should do about
// it. The zero value, ErrOther, means unclassified and therefore not retryable:
// retrying an error nobody understood is how one failure becomes a storm.
type ErrorKind uint8

const (
	ErrOther ErrorKind = iota // zero value: not retryable
	ErrRateLimit
	ErrPromptTooLarge
	ErrOverloaded
	ErrNoEligible
	ErrUsageLimit
	ErrTransient
)

// String returns the spelling a log line or an error message uses.
func (k ErrorKind) String() string {
	switch k {
	case ErrOther:
		return "other"
	case ErrRateLimit:
		return "rate_limit"
	case ErrPromptTooLarge:
		return "prompt_too_large"
	case ErrOverloaded:
		return "overloaded"
	case ErrNoEligible:
		return "no_eligible"
	case ErrUsageLimit:
		return "usage_limit"
	case ErrTransient:
		return "transient"
	}
	return fmt.Sprintf("ErrorKind(%d)", uint8(k))
}

// apiErrorBodyLimit is how much of an unparsed body an APIError's message
// carries: enough to identify the error, short enough not to paste a page into
// a log line.
const apiErrorBodyLimit = 300

// APIError is a provider's refusal: an HTTP response for a request that failed,
// or an in-band error event on a stream that had already started.
type APIError struct {
	Status     int         // 0 for an in-band stream error
	Header     http.Header // nil for an in-band stream error
	Body       []byte      // the response body, or the in-band event's JSON
	Kind       ErrorKind
	RetryAfter time.Duration
	Dialect    Dialect
	Message    string // the provider's error.message when one was parsed
}

// Error renders the error for a log line or a wrapped chain. The provider's
// parsed message wins over the body; an unparsed body is truncated so a huge
// response cannot dominate the message.
func (e *APIError) Error() string {
	detail := e.Message
	if detail == "" {
		body := e.Body
		if len(body) > apiErrorBodyLimit {
			body = body[:apiErrorBodyLimit]
		}
		detail = string(body)
	}
	return fmt.Sprintf("llm: %s api error: status %d (%s): %s", e.Dialect, e.Status, e.Kind, detail)
}
