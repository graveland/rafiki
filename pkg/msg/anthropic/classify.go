package anthropic

import (
	"net/http"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// promptTooLargePhrase is the API's wording for a request that outgrew the
// model's context. It arrives as prose inside an ordinary 400, with no
// machine-readable code to key on.
const promptTooLargePhrase = "prompt is too long"

// noEligibleMarkers are OpenRouter's wordings for "nothing can serve this
// request under its provider routing", matched case-insensitively. They are
// prose too, which is why the status is restricted: an unrelated 4xx quoting
// one of them (an echoed prompt, say) must not be read as a routing failure.
var noEligibleMarkers = []string{"no endpoints found", "no allowed providers"}

// overloadedStatus is the API's non-standard "overloaded" status, alongside the
// ordinary 5xx range.
const overloadedStatus = 529

// Classify turns an HTTP refusal into the action a sender should take.
//
// The rules are the ones the SDK-era client applied, in the same order, so a
// retry, a prompt trim and a routing failure keep landing on the same paths:
// a rate limit needs backpressure, an over-long prompt needs a trim, an
// exhausted routing spec needs a different provider, an overloaded upstream and
// any other 5xx need a retry, and everything else is the caller's problem.
// h is part of the codec's uniform surface and is not consulted: nothing here
// keys on a header, and a Retry-After is the caller's to read.
func Classify(status int, h http.Header, body []byte) msg.ErrorKind {
	lower := strings.ToLower(string(body))
	switch status {
	case http.StatusTooManyRequests:
		return msg.ErrRateLimit
	case http.StatusBadRequest:
		if strings.Contains(lower, promptTooLargePhrase) {
			return msg.ErrPromptTooLarge
		}
		if hasAny(lower, noEligibleMarkers) {
			return msg.ErrNoEligible
		}
	case http.StatusNotFound:
		if hasAny(lower, noEligibleMarkers) {
			return msg.ErrNoEligible
		}
	}
	if status == overloadedStatus || parseStreamError(body).ErrType == "overloaded_error" {
		return msg.ErrOverloaded
	}
	if status >= 500 && status <= 599 {
		return msg.ErrTransient
	}
	return msg.ErrOther
}

// classifyInBand classifies an SSE "error" event's payload, which has no status
// code: the event names its own kind.
func classifyInBand(data []byte) msg.ErrorKind {
	se := parseStreamError(data)
	switch {
	case se.ErrType == "rate_limit_error" || se.ErrType == "rate_limit_exceeded":
		return msg.ErrRateLimit
	case se.ErrType == "overloaded_error":
		return msg.ErrOverloaded
	case se.ErrType == "timeout_error" || se.ErrType == "api_error" || se.Code >= 500:
		return msg.ErrTransient
	case strings.Contains(strings.ToLower(se.Message), promptTooLargePhrase):
		return msg.ErrPromptTooLarge
	}
	return msg.ErrOther
}

func hasAny(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
