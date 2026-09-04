package main

import (
	"errors"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/darajapool"
)

const darajaStateLabel = "rafiki/daraja-state"

// WireDaraja registers OnConnect and OnDisconnect callbacks on the given pool
// so that a disconnected daraja marks its child unreachable in the list, and
// a reconnect clears the label. It also records the registry so Close and Kill
// can call Forget before the row vanishes.
func (c *Controller) WireDaraja(pool *darajapool.Pool, reg *darajapool.Registry) {
	if pool == nil || reg == nil {
		return
	}
	c.darajaReg = reg
	pool.OnConnect(c.onDarajaConnect)
	pool.OnDisconnect(c.onDarajaDisconnect)
}

// onDarajaConnect fires when a daraja (re-)connects for childID. It clears the
// unreachable label — if the child somehow survived a reconnect without being
// removed, leaving the label would be noise.
func (c *Controller) onDarajaConnect(childID string) {
	snap, ok := c.st.Get(childID)
	if !ok {
		// Child was closed/killed between disconnect and connect. Safe to drop.
		return
	}
	if snap.Labels[darajaStateLabel] == "" {
		// Already clean — no write needed.
		return
	}
	_, err := c.st.SetLabels(childID, map[string]string{}, []string{darajaStateLabel})
	if err != nil {
		slog.Warn("daraja: could not clear unreachable label",
			"childId", childID, "error", err)
	}
}

// onDarajaDisconnect fires when a daraja drops its connection for childID. It
// sets the unreachable label so rafiki list/get shows the child's true
// connectivity state rather than silently lying about streaming.
func (c *Controller) onDarajaDisconnect(childID string) {
	snap, ok := c.st.Get(childID)
	if !ok {
		// Child was already closed or exiting. The label write below would fail
		// ErrNotFound anyway — best effort, don't log.
		return
	}
	if snap.Labels[darajaStateLabel] != "" {
		// Already marked. Don't write a duplicate.
		return
	}
	_, err := c.st.SetLabels(childID, map[string]string{
		darajaStateLabel: "unreachable",
	}, nil)
	if err != nil && !errors.Is(err, childstore.ErrNotFound) {
		// The child may have been deleted concurrently by Close. Best-effort.
		slog.Warn("daraja: could not mark child unreachable",
			"childId", childID, "error", err)
	}
}
