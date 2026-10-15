package execpool

import "context"

// ownerKey is the context key under which WithOwner carries a child's id.
//
// It is a private struct type, not a string, for the usual reason: a string key
// is collidable with any other package's, and the context value rides every
// executor call.
type ownerKey struct{}

// WithOwner tags ctx with the child that owns an executor call. The daemon
// wraps the context ONCE, in boundExecutor's call path, so every Execute,
// StartJob and job-lifecycle event carries the owner without each call site
// remembering to.
//
// The owner is what lets an executor attribute a background job to the child
// that started it — it rides ExecuteRequest.Owner out, and the executor echoes
// it on every JobRunning/JobExited event so the daemon routes an event to its
// child with no handle bookkeeping of its own.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

// ownerFrom returns the owner WithOwner recorded, or "" when none was set. An
// untagged context is legitimate: the daemon's own calls (a claude child's
// proxy traffic, health) are not jobs and carry no owner.
func ownerFrom(ctx context.Context) string {
	s, _ := ctx.Value(ownerKey{}).(string)
	return s
}
