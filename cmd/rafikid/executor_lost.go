package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"
)

// lostExecutorNameTimeout bounds the one row read lostExecutorName does. It is
// short: the read is a single indexed lookup, and this runs on the pool's
// lost-executor callback, where a wedged store must not hold up the notice every
// other child of that machine is waiting for.
const lostExecutorNameTimeout = 3 * time.Second

// lostExecutorName names a lost executor in the steer its children receive: the
// machine label from its DURABLE row, else a short id. Never empty for a real
// id — a child told "executor  disconnected" has been told nothing.
//
// The durable row, not the live pool, and that is the whole point: pkg/execpool
// removes an executor from its live set BEFORE it parks it, so by the time the
// park expires and this runs, the binder's DisplayName can no longer see the
// machine label — it would answer a short id at exactly the moment the name
// matters. The row is the operator's copy and outlives the connection, which is
// why controllerBinder.WorkspaceMode reads it too.
//
// A missing row is an ANSWER (the executor was deleted) and falls back
// silently; a failed read is not, and is logged, because a database blip should
// not turn every machine in the fleet into a hex string without a trace.
func lostExecutorName(c *Controller, executorID string) string {
	if c.execStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), lostExecutorNameTimeout)
		defer cancel()
		row, err := c.execStore.Get(ctx, executorID)
		switch {
		case err == nil:
			if machine := row.Labels["machine"]; machine != "" {
				return machine
			}
		case !errors.Is(err, executors.ErrNotFound):
			slog.Warn("could not read the lost executor's row; naming it by id",
				"executorId", shortID(executorID), "error", err)
		}
	}
	return shortID(executorID)
}

// executorLostSteer is the steer a child gets when an executor it was using
// disconnects for good.
//
// It deliberately does NOT say "stop what you are doing": losing a machine
// never fails a child. Calls to that machine fail until it reconnects, and when
// it does the workspace there is rebuilt from the child's cwd — so anything the
// child never committed on that machine can be gone, which is what it has to be
// told before it reports work as done.
//
// name is lostExecutorName's. It is the only variable part, so the text stays
// one reviewable sentence set rather than a format string assembled at each
// call site.
func executorLostSteer(name string) string {
	return "EXECUTOR LOST: executor " + name + " disconnected. Calls to it fail until it reconnects; when it does, your workspace there is rebuilt and anything not committed on that machine may be gone. Your other tools, and any other executors, still work."
}
