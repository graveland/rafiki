package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
)

// UsageLimitError is a subscription usage limit: terminal for the turn, never
// retried, because the reset is hours or days away and retries only burn
// budget.
//
// It is deliberately NOT a *msg.APIError. A classifier keyed on a status code
// would otherwise read it as an ordinary 429 and retry it through a backoff that
// resets tomorrow, and one keyed on error text would retry anything that looks
// like a transport failure. A distinct type makes "the account is out of quota"
// impossible to confuse with "try again in a moment", and Error() deliberately
// avoids every fragment those fallbacks look for.
type UsageLimitError struct {
	Code     string    // usage_limit_reached, usage_not_included, insufficient_quota
	PlanType string    // the account's plan as the endpoint reports it; "" when unknown
	ResetsAt time.Time // zero = unknown
}

// usageLimitCodes are the error codes (or types) the backend answers with when
// the account, rather than the request, is out of budget.
var usageLimitCodes = map[string]bool{
	"usage_limit_reached": true,
	"usage_not_included":  true,
	"insufficient_quota":  true,
}

// codeContextLengthExceeded is the endpoint's code for a prompt that does not
// fit: in-band on the streaming path, and in the body of a 400 on the plain one.
const codeContextLengthExceeded = "context_length_exceeded"

// rateLimitCodes are the spellings the endpoint uses for "back off". Both are
// carried, because the two backends do not agree on the name: the ChatGPT
// dialect sends rate_limit_exceeded and an in-band error event may say
// rate_limit_error.
//
// It matters which kind this becomes. A rate limit needs BACKPRESSURE — the
// sender's rate-limit loop, which blocks every send for that model while it
// backs off — not a generic per-turn retry, and certainly not a terminal
// failure: an in-band rate limit classified as anything else either burns the
// turn's budget against a shared limit or fails a turn a pause would have
// rescued.
var rateLimitCodes = map[string]bool{
	"rate_limit_exceeded": true,
	"rate_limit_error":    true,
}

// overloadedCodes are the spellings the endpoint uses for "the upstream is
// saturated". All three are carried: an in-band error event may name
// overloaded_error, while the ChatGPT dialect's own code is server_is_overloaded
// and its backpressure hint is slow_down.
//
// These must reach ErrOverloaded rather than falling through to ErrOther. An
// overload is a transient upstream condition that a retry routinely survives, and
// ErrOther is terminal, so reading one as unclassified turns a retry into a hard
// failure at exactly the moment the provider is asking for patience.
var overloadedCodes = map[string]bool{
	"overloaded_error":     true,
	"server_is_overloaded": true,
	"slow_down":            true,
}

// Error renders the limit in the terms the operator can act on: which code, the
// plan, and when it lifts.
//
// The text says "provider usage limit", not "ChatGPT": the same type is produced
// for an api-key provider's insufficient_quota, and naming a product the request
// never reached would send the operator looking in the wrong place.
func (e *UsageLimitError) Error() string {
	plan := e.PlanType
	if plan == "" {
		plan = "unknown"
	}
	if e.ResetsAt.IsZero() {
		return fmt.Sprintf("provider usage limit reached (%s, plan %s); reset time unknown", e.Code, plan)
	}
	rest := time.Until(e.ResetsAt).Round(time.Minute)
	return fmt.Sprintf("provider usage limit reached (%s, plan %s); resets at %s (in %s)",
		e.Code, plan, e.ResetsAt.UTC().Format(time.RFC3339), rest)
}

// apiError is the error object the endpoint puts at the top level of an SSE
// "error" event, under "error" in an HTTP body, and under a failed response's
// "error".
type apiError struct {
	Code     json.RawMessage `json:"code"`
	Type     string          `json:"type"`
	Message  string          `json:"message"`
	PlanType string          `json:"plan_type"`
	ResetsAt *int64          `json:"resets_at"`
}

// apiErrorEnvelope accepts every spelling the endpoint uses: the nested
// {"error":{...}} of an HTTP body and of some SSE error events, the flat
// {"type":...,"code":...,"message":...} of others, and the response object a
// response.failed event carries its error inside.
type apiErrorEnvelope struct {
	Type     string          `json:"type"`
	Code     json.RawMessage `json:"code"`
	Message  string          `json:"message"`
	Error    *apiError       `json:"error"`
	Response *struct {
		Error *apiError `json:"error"`
	} `json:"response"`
}

// err returns the payload's error object, wherever this envelope shape put it.
func (env apiErrorEnvelope) err() *apiError {
	if env.Error != nil {
		return env.Error
	}
	if env.Response != nil {
		return env.Response.Error
	}
	return nil
}

// decodeErrorEnvelope parses an error payload leniently: a body that is not the
// expected JSON at all yields a zero envelope rather than an error, because the
// caller's fallback (an ordinary status-carrying error) is always correct.
func decodeErrorEnvelope(body []byte) apiErrorEnvelope {
	var env apiErrorEnvelope
	if len(body) == 0 {
		return env
	}
	_ = json.Unmarshal(body, &env)
	return env
}

// codes lists the names this payload carries, most specific first: the error
// object's code then type, then the flat code then type, deduped and with empties
// dropped.
//
// Both are listed because the endpoint does not keep the two fields distinct: a
// usage limit can arrive as type=usage_limit_reached with some other code, and
// reading only the code field turns exactly that into a retryable 429 that burns
// the turn's budget.
func (env apiErrorEnvelope) codes() []string {
	var raw []string
	if e := env.err(); e != nil {
		raw = append(raw, rawJSONCode(e.Code), e.Type)
	}
	raw = append(raw, rawJSONCode(env.Code), env.Type)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if c == "" || slices.Contains(out, c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// usageLimitCode reports the usage-limit code this payload carries and whether it
// carries one at all (see codes for why either field may be the one that names
// it).
func (env apiErrorEnvelope) usageLimitCode() (string, bool) {
	for _, c := range env.codes() {
		if usageLimitCodes[c] {
			return c, true
		}
	}
	return "", false
}

// info flattens the envelope into the fields the classifiers read, preferring
// the error object and falling back to the flat fields.
func (env apiErrorEnvelope) info() (code, planType, message string, resetsAt *int64) {
	if e := env.err(); e != nil {
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

// usageLimitFromEnvelope builds the terminal error for a payload that names a
// usage limit, or nil when it does not.
func usageLimitFromEnvelope(env apiErrorEnvelope) *UsageLimitError {
	code, ok := env.usageLimitCode()
	if !ok {
		return nil
	}
	_, planType, _, resetsAt := env.info()
	e := &UsageLimitError{Code: code, PlanType: planType}
	if resetsAt != nil {
		e.ResetsAt = time.Unix(*resetsAt, 0)
	}
	return e
}

// UsageLimitFromResponse builds the terminal error for an HTTP response, or nil
// when the response is not a usage limit.
//
// Only a 429 and a 404 can carry it: the ChatGPT backend answers a spent
// subscription with 429 normally, and some clients see the same body on a 404.
// Any other status stays an ordinary retryable or terminal error, and a 429
// without a usage-limit code stays a retryable 429.
func UsageLimitFromResponse(status int, header http.Header, body []byte) *UsageLimitError {
	if status != http.StatusTooManyRequests && status != http.StatusNotFound {
		return nil
	}
	env := decodeErrorEnvelope(body)
	code, ok := env.usageLimitCode()
	if !ok {
		return nil
	}
	_, planType, _, resetsAt := env.info()
	if planType == "" {
		planType = header.Get("x-codex-plan-type")
	}
	e := &UsageLimitError{Code: code, PlanType: planType}
	if resetsAt != nil {
		e.ResetsAt = time.Unix(*resetsAt, 0)
	} else if t := resetTime(header, time.Now()); !t.IsZero() {
		e.ResetsAt = t
	}
	return e
}

// resetTime reads the reset instant from the rate-limit headers the ChatGPT
// backend sends alongside every response. The header FAMILY depends on which
// window is currently binding: x-codex-active-limit says "codex_other" when the
// other (secondary) window is the one in force, and that family's headers are the
// ones carrying its reset. Hardcoding the primary pair reports the wrong reset
// time whenever the other window is the one that ran out.
func resetTime(header http.Header, now time.Time) time.Time {
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

// Classify classifies an HTTP status and body, or an in-band stream error
// payload (status 0, header nil), into what a sender should do about it.
//
// The body's codes win over the status, because the endpoint reports the
// account-level conditions under an ordinary status: a usage limit normally
// arrives as a 429 (and is never a rate limit), a prompt that does not fit as a
// 400 whose body names context_length_exceeded, and an in-band rate limit or
// overload as an error event with no status at all. Everything unclassified is
// ErrOther, which is not retryable: retrying an error nobody understood is how
// one failure becomes a storm.
//
// h is part of the codec's uniform surface and is not consulted: nothing here
// keys on a header, and a Retry-After is the caller's to read.
func Classify(status int, h http.Header, body []byte) msg.ErrorKind {
	env := decodeErrorEnvelope(body)
	if _, ok := env.usageLimitCode(); ok {
		return msg.ErrUsageLimit
	}
	for _, c := range env.codes() {
		switch {
		case c == codeContextLengthExceeded:
			return msg.ErrPromptTooLarge
		case rateLimitCodes[c]:
			return msg.ErrRateLimit
		case overloadedCodes[c]:
			return msg.ErrOverloaded
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return msg.ErrRateLimit
	case status >= 500 && status <= 599:
		return msg.ErrTransient
	}
	return msg.ErrOther
}
