// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/store"
)

// trackLease records a lease this daemon holds for a child.
func (c *Controller) trackLease(childID string, l store.Lease) {
	c.heldLeasesMu.Lock()
	c.heldLeases[childID] = l
	c.heldLeasesMu.Unlock()
}

// dropLease forgets a lease without releasing it. Used when a resume fails —
// the lease expires on its own, which is the right outcome for a child this
// daemon could not actually start.
func (c *Controller) dropLease(childID string) {
	c.heldLeasesMu.Lock()
	delete(c.heldLeases, childID)
	c.heldLeasesMu.Unlock()
}

// startLeaseRenewal keeps every held lease alive until the daemon stops.
//
// Renewal runs on its own goroutine, independent of turn progress. A thinking
// phase can exceed any sane TTL, so coupling renewal to turn completion would
// drop the lease of a perfectly healthy child mid-turn.
func (c *Controller) startLeaseRenewal(ctx context.Context) {
	if c.leases == nil {
		return
	}
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
		ticker := time.NewTicker(leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				c.releaseAllLeases()
				return
			case <-ticker.C:
				c.renewLeasesOnce(ctx)
			}
		}
	}()
}

// renewLeasesOnce renews every held lease, stopping any child whose lease is
// gone.
func (c *Controller) renewLeasesOnce(ctx context.Context) {
	c.heldLeasesMu.Lock()
	snapshot := make(map[string]store.Lease, len(c.heldLeases))
	for k, v := range c.heldLeases {
		snapshot[k] = v
	}
	c.heldLeasesMu.Unlock()

	for childID, lease := range snapshot {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ok, err := c.leases.Renew(rctx, lease, leaseTTL)
		cancel()
		if err != nil {
			// A transient database error is not an answer. Keep the lease and
			// try again on the next tick — there are five attempts inside one
			// TTL precisely so a single failure costs nothing.
			slog.Warn("lease renewal failed; will retry", "childId", childID, "error", err)
			continue
		}
		if !ok {
			c.onLeaseLost(childID)
		}
	}
}

// onLeaseLost stops a child whose conversation was taken over.
//
// The database guard fences WRITES; it does not fence SIDE EFFECTS. An engine
// that keeps looping after losing its lease keeps calling the LLM and keeps
// running bash and write against a workspace, while nothing it does can be
// recorded. Two agents acting on the world when only one can write down what it
// did is worse than either a double write or a hard stop.
//
// No steer frame and no graceful wind-down: the child cannot record a farewell,
// because recording is exactly what it has lost the right to do.
func (c *Controller) onLeaseLost(childID string) {
	c.dropLease(childID)

	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	slog.Warn("conversation lease lost; stopping child",
		"childId", childID, "conversationId", snap.SessionID)

	if err := c.st.Update(childID, func(s *childstore.Session) {
		s.LastRetryError = "lease lost"
	}); err != nil {
		slog.Warn("record lease-lost reason", "childId", childID, "error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Kill(ctx, childID, 5000, 5000); err != nil {
		slog.Error("stopping a child after lease loss failed", "childId", childID, "error", err)
	}
}

// releaseAllLeases drops every held lease on clean shutdown, so a restarting
// daemon's peers see the conversations as free immediately rather than after
// the TTL.
func (c *Controller) releaseAllLeases() {
	c.heldLeasesMu.Lock()
	snapshot := make([]store.Lease, 0, len(c.heldLeases))
	for _, v := range c.heldLeases {
		snapshot = append(snapshot, v)
	}
	c.heldLeases = make(map[string]store.Lease)
	c.heldLeasesMu.Unlock()

	for _, lease := range snapshot {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.leases.Release(ctx, lease); err != nil {
			slog.Warn("release lease", "conversationId", lease.ConversationID, "error", err)
		}
		cancel()
	}
}
