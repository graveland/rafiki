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

// OwnerFrom returns the owner WithOwner recorded, or "" when none was set. An
// untagged context is legitimate: the daemon's own calls (a claude child's
// proxy traffic, health) are not jobs and carry no owner.
//
// Exported so a caller that must prove the owner actually reached its executor
// call can read it back — the private context key means an out-of-package test
// otherwise cannot observe what WithOwner set, which is exactly how a wrapper
// that tags its own ctx copy while the callee closes over the caller's ctx went
// unnoticed.
func OwnerFrom(ctx context.Context) string {
	s, _ := ctx.Value(ownerKey{}).(string)
	return s
}
