package executors

import "errors"

// The rejections a Store may return from Enroll or Authenticate.
//
// They live HERE, in the interface package, rather than alongside the Postgres
// implementation that produces them, because the pool has to distinguish them
// and the pool must not link a database driver: pkg/execpool carries the
// executor's own dial path too, and cmd/rafiki-executor links it.
// pkg/executorsdb aliases these, so every existing call site keeps working.
var (
	// ErrTokenUnknown means the enrollment token does not exist.
	ErrTokenUnknown = errors.New("executor: enrollment token unknown")
	// ErrTokenConsumed means the enrollment token was already redeemed. A
	// one-time token is one-time; a second use is a different machine.
	ErrTokenConsumed = errors.New("executor: enrollment token already consumed")
	// ErrTokenExpired means the enrollment window closed.
	ErrTokenExpired = errors.New("executor: enrollment token expired")
	// ErrDisabled means the row exists and is switched off — a revocation.
	ErrDisabled = errors.New("executor: disabled")
	// ErrNotFound means no row matches.
	ErrNotFound = errors.New("executor: not found")
)

// IsTerminalAuthError reports whether err is a decision about the CREDENTIAL
// rather than a failure to reach the thing that stores it.
//
// The distinction is the whole point. "This credential is not valid" is an
// answer, and an executor that receives it should stop: retrying cannot change
// a revoked row, and a machine spinning on a rejected credential is noise at
// best. "I could not check" is not an answer — a database blip, a network
// partition, a restarting Postgres — and an executor that treats it as one
// takes itself out of service permanently over a condition that resolves in
// seconds. Applied across a fleet reconnecting at the same moment, that is the
// difference between a blip and an outage.
//
// It fails toward RETRY: an error nobody classified is assumed transient. The
// cost of retrying a genuinely dead credential is a log line and some backoff;
// the cost of quitting on a transient one is the whole fleet.
func IsTerminalAuthError(err error) bool {
	switch {
	case errors.Is(err, ErrTokenUnknown),
		errors.Is(err, ErrTokenConsumed),
		errors.Is(err, ErrTokenExpired),
		errors.Is(err, ErrDisabled),
		errors.Is(err, ErrNotFound):
		return true
	default:
		return false
	}
}
