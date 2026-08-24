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
	// ErrMachineNameTaken means the row this enrollment would create collides
	// with an existing executor on (owner, machine) -- the operator minted two
	// tokens with the same --name.
	//
	// Its TEXT is the operator-facing message, deliberately. writeAuthFailure
	// forwards err.Error() verbatim for a terminal error, and the peer on the
	// other end has not proved who it is, so the store's own error must never
	// reach it: a pgx message carries the DSN. pkg/executorsdb therefore
	// DISCARDS the pgconn error on this path and returns this sentinel bare.
	//
	// It is also why this sentinel exists at all rather than the raw 23505
	// travelling: unclassified, IsTerminalAuthError falls through to retryable
	// and the executor reconnects forever against a name it can never claim.
	ErrMachineNameTaken = errors.New("executor: this machine name is already " +
		"claimed by another executor for this owner; mint a token with a " +
		"different --name, or relabel the existing executor")
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
		errors.Is(err, ErrNotFound),
		// A name collision is an ANSWER: retrying cannot make the name free,
		// and the executor that already holds it is the one still running.
		// Left unclassified this is retryable, and the loser reconnects
		// forever while the daemon logs "could not verify an executor
		// credential" on every attempt -- so the operator sees an executor
		// that simply never appears, with nothing naming the real cause.
		errors.Is(err, ErrMachineNameTaken):
		return true
	default:
		return false
	}
}
